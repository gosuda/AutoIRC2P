<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import { messageKey, type Language, type Message, type Outgoing } from './api';
  import { copy, languageName } from './i18n';

  let { language, translationEnabled, autoTranslate, roomName, messages, outgoing, loading, error, sendsError, checking, onread, onretry, oncheck, onrestore }: {
    language: Language;
    translationEnabled: boolean;
    autoTranslate: boolean;
    roomName: string;
    messages: Message[];
    outgoing: Outgoing[];
    loading: boolean;
    error: string;
    sendsError: string;
    checking: Record<string, boolean>;
    onread: (roomName: string, messageId: number) => void;
    onretry: () => void;
    oncheck: (send: Outgoing) => void;
    onrestore: (send: Outgoing) => void;
  } = $props();
  let viewport: HTMLDivElement;
  let atBottom = $state(true);
  let disposed = false;
  let metadata: HTMLDivElement;
  let detailsAnchor: HTMLElement | undefined;
  let details = $state<Message | Outgoing | null>(null);
  let holdTimer: number | undefined;
  let press: { id: number; x: number; y: number } | undefined;
  const text = $derived(copy[language]);
  const timeFormat = $derived(new Intl.DateTimeFormat(language, { hour: '2-digit', minute: '2-digit' }));
  const pending = $derived(outgoing.filter((send) => !messages.some((message) => message.own && (message.id === send.messageId || message.requestId === send.requestId))));

  function cancelPress() {
    clearTimeout(holdTimer);
    press = undefined;
  }

  function hideDetails() {
    cancelPress();
    if (!metadata?.matches(':popover-open')) return;
    const restoreFocus = metadata.contains(document.activeElement);
    metadata.hidePopover();
    if (restoreFocus) detailsAnchor?.querySelector('button')?.focus({ preventScroll: true });
  }

  async function showDetails(message: Message | Outgoing, anchor: HTMLElement) {
    cancelPress();
    details = message;
    await tick();
    if (disposed || !anchor.isConnected) return;
    detailsAnchor = anchor;
    metadata.showPopover();
    const bounds = anchor.getBoundingClientRect();
    const panel = metadata.getBoundingClientRect();
    metadata.style.left = `${Math.max(8, Math.min(bounds.left, window.innerWidth - panel.width - 8))}px`;
    metadata.style.top = `${bounds.bottom + panel.height + 8 <= window.innerHeight ? bounds.bottom + 8 : Math.max(8, bounds.top - panel.height - 8)}px`;
    metadata.querySelector('button')?.focus({ preventScroll: true });
  }

  function pointerDown(event: PointerEvent, message: Message | Outgoing) {
    if (event.button !== 0 || !event.isPrimary || (event.target as HTMLElement).closest('button, a')) return;
    cancelPress();
    const anchor = event.currentTarget as HTMLElement;
    press = { id: event.pointerId, x: event.clientX, y: event.clientY };
    anchor.setPointerCapture(event.pointerId);
    holdTimer = window.setTimeout(() => { void showDetails(message, anchor); }, 650);
  }

  function pointerMove(event: PointerEvent) {
    if (press?.id === event.pointerId && Math.hypot(event.clientX - press.x, event.clientY - press.y) > 8) cancelPress();
  }

  function contextMenu(event: MouseEvent, message: Message | Outgoing) {
    event.preventDefault();
    void showDetails(message, event.currentTarget as HTMLElement);
  }

  $effect(() => { autoTranslate; hideDetails(); });

  function isAtBottom() {
    return viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight < 24;
  }

  function trackScroll() {
    hideDetails();
    atBottom = isAtBottom();
    reportRead();
  }

  function reportRead() {
    if (!viewport || disposed) return;
    if (!isAtBottom() || loading || error || document.visibilityState !== 'visible' || !document.hasFocus()) return;
    const latest = messages.reduce((id, message) => !message.service ? Math.max(id, message.id) : id, 0);
    if (latest > 0) onread(roomName, latest);
  }

  $effect(() => {
    messages;
    outgoing;
    loading;
    autoTranslate;
    const follow = untrack(() => atBottom);
    void tick().then(() => {
      if (disposed || !viewport) return;
      if (follow) viewport.scrollTop = viewport.scrollHeight;
      reportRead();
    });
  });

  onMount(() => {
    const resize = new ResizeObserver(() => {
      if (atBottom) viewport.scrollTop = viewport.scrollHeight;
      reportRead();
    });
    resize.observe(viewport);
    window.addEventListener('focus', reportRead);
    document.addEventListener('visibilitychange', reportRead);
    return () => {
      disposed = true;
      cancelPress();
      resize.disconnect();
      window.removeEventListener('focus', reportRead);
      document.removeEventListener('visibilitychange', reportRead);
    };
  });
</script>

{#snippet sentCheck()}
  <svg class="sent-check" viewBox="0 0 20 20" role="img" aria-label={text.sent}><title>{text.sent}</title><path d="m4 10 4 4 8-8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" /></svg>
{/snippet}

<svelte:window onresize={hideDetails} onblur={hideDetails} onkeydown={(event) => { if (event.key === 'Escape') hideDetails(); }} onpointerdown={(event) => { if (metadata && !metadata.contains(event.target as Node)) metadata.hidePopover(); }} />

<div bind:this={metadata} popover="manual" class="message-metadata" role="dialog" aria-label={text.messageDetails}>
  {#if details}
    <header><strong>{text.messageDetails}</strong><button type="button" class="text-button" onclick={hideDetails}>{text.close}</button></header>
    {#if autoTranslate && 'targetLanguage' in details && details.targetLanguage}
      <p class="message-language">{text.translation}: {languageName(details.targetLanguage, language)}</p>
    {/if}
    <time datetime={details.createdAt}>{new Date(details.createdAt).toLocaleString(language)}</time>
    <div class="original-content"><span class="original-label">{text.original}</span><p class="message-original" dir="auto">{details.original}</p></div>
  {/if}
</div>

<div class="feed-shell">
  <div bind:this={viewport} class="message-feed" role="region" aria-label={text.messages} aria-busy={loading} onscroll={trackScroll}>
    <div role="log" aria-label={text.messages} aria-live="polite" aria-relevant="additions text">
      {#if error || sendsError}
        <div class="history-error" role="status"><strong>{error ? text.historyError : text.sendsError}</strong><p>{error || sendsError}</p><button type="button" onclick={onretry} disabled={loading}>{text.retry}</button></div>
      {/if}
      {#if loading && messages.length === 0}
        <div class="empty-state"><span class="empty-mark" aria-hidden="true">#</span><p>{text.loadingHistory}</p></div>
      {:else if messages.length === 0 && pending.length === 0 && !error}
        <div class="empty-state"><span class="empty-mark" aria-hidden="true">#</span><h2>{text.emptyTitle}</h2><p>{text.emptyDescription}</p></div>
      {/if}
      {#each messages as message (messageKey(message))}
        <article class="message" class:own-message={message.own} class:service-message={message.service} onpointerdown={(event) => pointerDown(event, message)} onpointermove={pointerMove} onpointerup={cancelPress} onpointercancel={cancelPress} onlostpointercapture={cancelPress} oncontextmenu={(event) => contextMenu(event, message)}>
          <header class="message-header">
            <button type="button" class="message-nick message-details-trigger" title={text.metadataHint} aria-label={`${text.messageDetails}: ${message.nick}`} onclick={(event) => void showDetails(message, event.currentTarget.closest('article')!)}>{message.own ? text.you : message.nick}</button>
            {#if !autoTranslate}<time datetime={message.createdAt} title={new Date(message.createdAt).toLocaleString(language)}>{timeFormat.format(new Date(message.createdAt))}</time>{/if}
            {#if message.own}{@render sentCheck()}{/if}
          </header>
          {#if !autoTranslate}
            <p class="message-translation" dir="auto">{message.original}</p>
          {:else}
            <div class="translated-content">
              {#if message.translationState === 'ready' && message.translation}
                <p class="message-translation" lang={message.targetLanguage || language} dir="auto">{message.translation}</p>
              {:else if message.translationState === 'pending'}
                <p class="translation-status pending">{text.pending}</p>
                <p class="message-original" dir="auto">{message.original}</p>
              {:else if message.translationState === 'excluded' || message.service}
                <p class="translation-status">{message.service ? text.service : text.excluded}</p>
              {:else}
                <p class="translation-status failed">{text.failed}</p>
              {/if}
            </div>
          {/if}
        </article>
      {/each}
      {#each pending as send (send.requestId)}
        <article class="message own-message outgoing-message" class:outgoing-error={send.state === 'failed' || send.state === 'unconfirmed'} aria-label={text.outgoing} onpointerdown={(event) => pointerDown(event, send)} onpointermove={pointerMove} onpointerup={cancelPress} onpointercancel={cancelPress} onlostpointercapture={cancelPress} oncontextmenu={(event) => contextMenu(event, send)}>
          <header class="message-header"><button type="button" class="message-nick message-details-trigger" title={text.metadataHint} aria-label={text.messageDetails} onclick={(event) => void showDetails(send, event.currentTarget.closest('article')!)}>{text.you}</button>{#if !autoTranslate}<time datetime={send.createdAt}>{timeFormat.format(new Date(send.createdAt))}</time>{/if}{#if send.state === 'confirmed'}{@render sentCheck()}{/if}</header>
          {#if !autoTranslate || send.state === 'translating'}<p class="message-original" dir="auto">{send.original}</p>{/if}
          {#if send.state !== 'confirmed'}
            <div class="outgoing-status" role="status">
              {#if send.state === 'translating'}{translationEnabled ? text.translating : text.sending}
              {:else if send.state === 'sending' || send.state === 'awaiting_echo'}{text.awaitingEcho}
              {:else if send.state === 'failed'}{text.sendFailed}
              {:else}{text.unconfirmed}{/if}
            </div>
            <div class="outgoing-actions">
              {#if send.state === 'unconfirmed' || send.state === 'awaiting_echo'}<button type="button" class="text-button" disabled={checking[send.requestId]} onclick={() => oncheck(send)}>{checking[send.requestId] ? text.checking : text.checkStatus}</button>{/if}
              {#if send.state === 'failed' || send.state === 'unconfirmed'}<button type="button" class="text-button" onclick={() => onrestore(send)}>{text.restoreDraft}</button>{/if}
            </div>
          {/if}
        </article>
      {/each}
    </div>
  </div>
  {#if !atBottom && (messages.length > 0 || pending.length > 0)}
    <button type="button" class="latest-button" onclick={() => { atBottom = true; viewport.scrollTop = viewport.scrollHeight; reportRead(); }}>{text.latest} <span aria-hidden="true">↓</span></button>
  {/if}
</div>
