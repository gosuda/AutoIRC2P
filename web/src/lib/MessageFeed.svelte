<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import { messageKey, type Language, type Message, type Outgoing } from './api';
  import { copy, languageName } from './i18n';

  let { language, autoTranslate, roomName, messages, outgoing, loading, error, sendsError, checking, onread, onretry, oncheck, onrestore }: {
    language: Language;
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
  const text = $derived(copy[language]);
  const timeFormat = $derived(new Intl.DateTimeFormat(language, { hour: '2-digit', minute: '2-digit' }));
  const pending = $derived(outgoing.filter((send) => !messages.some((message) => message.own && (message.id === send.messageId || message.requestId === send.requestId))));

  function isAtBottom() {
    return viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight < 24;
  }

  function trackScroll() {
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
      resize.disconnect();
      window.removeEventListener('focus', reportRead);
      document.removeEventListener('visibilitychange', reportRead);
    };
  });
</script>

{#snippet sentCheck()}
  <svg class="sent-check" viewBox="0 0 20 20" role="img" aria-label={text.sent}><title>{text.sent}</title><path d="m4 10 4 4 8-8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" /></svg>
{/snippet}

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
        <article class="message" class:own-message={message.own} class:service-message={message.service}>
          <header class="message-header">
            <span class="message-nick">{message.own ? text.you : message.nick}</span>
            {#if autoTranslate}<span class="message-language">{languageName(message.sourceLanguage, language)} <span aria-hidden="true">→</span> {languageName(message.targetLanguage || language, language)}</span>{/if}
            <time datetime={message.createdAt} title={new Date(message.createdAt).toLocaleString(language)}>{timeFormat.format(new Date(message.createdAt))}</time>
            {#if message.own}{@render sentCheck()}{/if}
          </header>
          {#if !autoTranslate}
            <p class="message-translation" lang={message.sourceLanguage || undefined} dir="auto">{message.original}</p>
          {:else if message.service}
            <p class="translation-status">{text.service}</p>
            <p class="message-original service-original" lang={message.sourceLanguage || undefined} dir="auto">{message.original}</p>
          {:else}
            <div class="translated-content">
              {#if message.translationState === 'ready' && message.translation}
                <p class="message-translation" lang={message.targetLanguage || language} dir="auto">{message.translation}</p>
              {:else if message.translationState === 'pending'}
                <p class="translation-status pending">{text.pending}</p>
              {:else if message.translationState === 'excluded'}
                <p class="translation-status">{text.excluded}</p>
              {:else}
                <p class="translation-status failed">{text.failed}</p>
              {/if}
            </div>
            <div class="original-content"><span class="original-label">{text.original}</span><p class="message-original" lang={message.sourceLanguage || undefined} dir="auto">{message.original}</p></div>
          {/if}
        </article>
      {/each}
      {#each pending as send (send.requestId)}
        <article class="message own-message outgoing-message" class:outgoing-error={send.state === 'failed' || send.state === 'unconfirmed'} aria-label={text.outgoing}>
          <header class="message-header"><span class="message-nick">{text.you}</span><time datetime={send.createdAt}>{timeFormat.format(new Date(send.createdAt))}</time>{#if send.state === 'confirmed'}{@render sentCheck()}{/if}</header>
          <p class="message-original" dir="auto">{send.original}</p>
          {#if send.state !== 'confirmed'}
            <div class="outgoing-status" role="status">
              {#if send.state === 'translating'}{text.translating}
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
