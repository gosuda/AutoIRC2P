<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import { messageKey, type Language, type Message, type Outgoing } from './api';
  import { copy, languageName } from './i18n';

  let { language, translationEnabled, autoTranslate, roomName, messages, outgoing, loading, error, sendsError, checking, recovering, submitting, ready, onread, onretry, oncheck, onrestore, onresend, ondelete }: {
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
    recovering: Record<string, boolean>;
    submitting: Record<string, boolean>;
    ready: boolean;
    onread: (roomName: string, messageId: number) => void;
    onretry: () => void;
    oncheck: (send: Outgoing) => void;
    onrestore: (send: Outgoing) => void;
    onresend: (send: Outgoing) => void;
    ondelete: (send: Outgoing) => void;
  } = $props();
  type FeedItem = { kind: 'message'; key: string; createdAt: string; dayKey: string; showDay: boolean; message: Message }
    | { kind: 'send'; key: string; createdAt: string; dayKey: string; showDay: boolean; send: Outgoing };

  let viewport: HTMLDivElement;
  let atBottom = $state(true);
  let disposed = false;
  let metadata: HTMLDivElement;
  let detailsAnchor: HTMLElement | undefined;
  let detailsSelection = $state<{ kind: 'message'; key: number | string } | { kind: 'send'; requestId: string } | null>(null);
  let holdTimer: number | undefined;
  let press: { id: number; x: number; y: number } | undefined;
  const text = $derived(copy[language]);
  const timeFormat = $derived(new Intl.DateTimeFormat(language, { hour: '2-digit', minute: '2-digit' }));
  const dateFormat = $derived(new Intl.DateTimeFormat(language, { year: 'numeric', month: 'short', day: 'numeric', weekday: 'short' }));
  const latestPersistedMessageId = $derived(messages.reduce((id, message) => message.id > id ? message.id : id, 0));
  const pending = $derived(outgoing.filter((send) => {
    if (messages.some((message) => message.own && (message.id === send.messageId || (send.requestId !== '' && message.requestId === send.requestId)))) {
      return false;
    }
    if (send.state === 'confirmed') {
      if (loading && messages.length === 0) return false;
      if (send.messageId <= 0 || send.messageId <= latestPersistedMessageId) return false;
    }
    return true;
  }));

  function localDayKey(iso: string): string {
    const date = new Date(iso);
    if (Number.isNaN(date.getTime())) return '';
    return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`;
  }

  function formatDay(iso: string): string {
    const date = new Date(iso);
    return Number.isNaN(date.getTime()) ? iso : dateFormat.format(date);
  }

  function formatTime(iso: string): string {
    const date = new Date(iso);
    return Number.isNaN(date.getTime()) ? '' : timeFormat.format(date);
  }

  function formatDateTime(iso: string): string {
    const date = new Date(iso);
    return Number.isNaN(date.getTime()) ? iso : date.toLocaleString(language);
  }

  const timeline = $derived.by(() => {
    const raw: Array<{ kind: 'message'; key: string; createdAt: string; timeMs: number; message: Message } | { kind: 'send'; key: string; createdAt: string; timeMs: number; send: Outgoing }> = [];
    for (const message of messages) {
      const timeMs = Date.parse(message.createdAt);
      raw.push({ kind: 'message', key: `msg:${messageKey(message)}`, createdAt: message.createdAt, timeMs: Number.isNaN(timeMs) ? 0 : timeMs, message });
    }
    for (const send of pending) {
      const timeMs = Date.parse(send.createdAt);
      raw.push({ kind: 'send', key: `send:${send.requestId}`, createdAt: send.createdAt, timeMs: Number.isNaN(timeMs) ? 0 : timeMs, send });
    }
    raw.sort((a, b) => {
      if (a.timeMs !== b.timeMs) return a.timeMs - b.timeMs;
      if (a.kind === 'message' && b.kind === 'message') {
        if (a.message.id < 0 && b.message.id < 0) return b.message.id - a.message.id;
        return a.message.id - b.message.id;
      }
      if (a.kind === 'send' && b.kind === 'send') {
        return (a.send.messageId - b.send.messageId) || a.send.requestId.localeCompare(b.send.requestId);
      }
      return a.kind === 'message' ? -1 : 1;
    });
    const items: FeedItem[] = [];
    let previousDay = '';
    for (const entry of raw) {
      const dayKey = localDayKey(entry.createdAt);
      const showDay = dayKey !== '' && dayKey !== previousDay;
      if (dayKey !== '') previousDay = dayKey;
      if (entry.kind === 'message') {
        items.push({ kind: 'message', key: entry.key, createdAt: entry.createdAt, dayKey, showDay, message: entry.message });
      } else {
        items.push({ kind: 'send', key: entry.key, createdAt: entry.createdAt, dayKey, showDay, send: entry.send });
      }
    }
    return items;
  });

  const details = $derived.by<Message | Outgoing | null>(() => {
    const selection = detailsSelection;
    if (!selection) return null;
    if (selection.kind === 'message') {
      return messages.find((candidate) => messageKey(candidate) === selection.key) ?? null;
    }
    return outgoing.find((candidate) => candidate.requestId === selection.requestId) ?? null;
  });

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

  async function showDetails(item: Message | Outgoing, anchor: HTMLElement) {
    cancelPress();
    detailsSelection = 'requestId' in item && !('id' in item)
      ? { kind: 'send', requestId: item.requestId }
      : { kind: 'message', key: messageKey(item as Message) };
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

<svelte:window onresize={hideDetails} onblur={hideDetails} onkeydown={(event) => { if (event.key === 'Escape') hideDetails(); }} onpointerdown={(event) => { if (metadata && !metadata.contains(event.target as Node)) hideDetails(); }} />

<div bind:this={metadata} popover="manual" class="message-metadata" role="dialog" aria-label={text.messageDetails}>
  {#if details}
    <header><strong>{text.messageDetails}</strong><button type="button" class="text-button" onclick={hideDetails}>{text.close}</button></header>
    {#if autoTranslate && 'targetLanguage' in details && details.targetLanguage}
      <p class="message-language">{text.translation}: {languageName(details.targetLanguage, language)}</p>
    {/if}
    <time datetime={details.createdAt}>{formatDateTime(details.createdAt)}</time>
    <div class="original-content"><span class="original-label">{text.original}</span><p class="message-original" dir="auto">{details.original}</p></div>
  {/if}
</div>

<div class="feed-shell">
  <div bind:this={viewport} class="message-feed" role="region" aria-label={text.messages} aria-busy={loading} onscroll={trackScroll}>
    <div role="log" aria-label={text.messages} aria-live="polite" aria-relevant="additions text">
      {#if error || sendsError}
        <div class="history-error" role="status"><strong>{error ? text.historyError : text.sendsError}</strong><p>{error || sendsError}</p><button type="button" onclick={onretry} disabled={loading}>{text.retry}</button></div>
      {/if}
      {#if loading && timeline.length === 0}
        <div class="empty-state"><span class="empty-mark" aria-hidden="true">#</span><p>{text.loadingHistory}</p></div>
      {:else if timeline.length === 0 && !error}
        <div class="empty-state"><span class="empty-mark" aria-hidden="true">#</span><h2>{text.emptyTitle}</h2><p>{text.emptyDescription}</p></div>
      {/if}
      {#each timeline as item (item.key)}
        {#if item.showDay}
          <div class="date-divider" role="separator" aria-label={formatDay(item.createdAt)}><span>{formatDay(item.createdAt)}</span></div>
        {/if}
        {#if item.kind === 'message'}
          {@const message = item.message}
          <article class="message" class:own-message={message.own} class:service-message={message.service} onpointerdown={(event) => pointerDown(event, message)} onpointermove={pointerMove} onpointerup={cancelPress} onpointercancel={cancelPress} onlostpointercapture={cancelPress} oncontextmenu={(event) => contextMenu(event, message)}>
            <header class="message-header">
              <button type="button" class="message-nick message-details-trigger" title={text.metadataHint} aria-label={`${text.messageDetails}: ${message.nick}`} onclick={(event) => void showDetails(message, event.currentTarget.closest('article')!)}>{message.own ? text.you : message.nick}</button>
              <time datetime={message.createdAt} title={formatDateTime(message.createdAt)}>{formatTime(message.createdAt)}</time>
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
                  <p class="message-original" dir="auto">{message.original}</p>
                {:else}
                  <p class="translation-status failed">{text.failed}</p>
                  <p class="message-original" dir="auto">{message.original}</p>
                {/if}
              </div>
            {/if}
          </article>
        {:else}
          {@const send = item.send}
          <article class="message own-message outgoing-message" class:outgoing-error={send.state === 'failed' || send.state === 'unconfirmed'} aria-label={text.outgoing} onpointerdown={(event) => pointerDown(event, send)} onpointermove={pointerMove} onpointerup={cancelPress} onpointercancel={cancelPress} onlostpointercapture={cancelPress} oncontextmenu={(event) => contextMenu(event, send)}>
            <header class="message-header"><button type="button" class="message-nick message-details-trigger" title={text.metadataHint} aria-label={text.messageDetails} onclick={(event) => void showDetails(send, event.currentTarget.closest('article')!)}>{text.you}</button><time datetime={send.createdAt} title={formatDateTime(send.createdAt)}>{formatTime(send.createdAt)}</time>{#if send.state === 'confirmed'}{@render sentCheck()}{/if}</header>
            <p class:message-translation={send.state === 'confirmed'} class:message-original={send.state !== 'confirmed'} dir="auto">{send.original}</p>
            {#if send.state !== 'confirmed'}
              <div class="outgoing-status" role="status">
                {#if send.state === 'translating'}{translationEnabled ? text.translating : text.sending}
                {:else if send.state === 'sending' || send.state === 'awaiting_echo'}{text.awaitingEcho}
                {:else if send.state === 'failed'}{text.sendFailed}
                {:else}{text.unconfirmed}{/if}
              </div>
              <div class="outgoing-actions">
                {#if send.state === 'unconfirmed' || send.state === 'awaiting_echo'}<button type="button" class="text-button" disabled={checking[send.requestId] || recovering[send.requestId]} onclick={() => oncheck(send)}>{checking[send.requestId] ? text.checking : text.checkStatus}</button>{/if}
                {#if send.state === 'failed' || send.state === 'unconfirmed'}
                  <button type="button" class="text-button" disabled={!ready || recovering[send.requestId] || checking[send.requestId] || submitting[send.room]} onclick={() => onresend(send)}>{text.resend}</button>
                  <button type="button" class="text-button" disabled={recovering[send.requestId] || checking[send.requestId] || submitting[send.room]} onclick={() => ondelete(send)}>{text.deleteSend}</button>
                  <button type="button" class="text-button" onclick={() => onrestore(send)}>{text.restoreDraft}</button>
                {/if}
              </div>
            {/if}
          </article>
        {/if}
      {/each}
    </div>
  </div>
  {#if !atBottom && timeline.length > 0}
    <button type="button" class="latest-button" onclick={() => { atBottom = true; viewport.scrollTop = viewport.scrollHeight; reportRead(); }}>{text.latest} <span aria-hidden="true">↓</span></button>
  {/if}
</div>
