<script lang="ts">
  import { onDestroy } from 'svelte';
  import type { Language, Room, User } from './api';
  import { copy, languageName } from './i18n';

  let { language, room, user, draft, ready, busy, error, onlogin, ondraft, onsend }: {
    language: Language;
    room: Room;
    user: User | null;
    draft: string;
    ready: boolean;
    busy: boolean;
    error: string;
    onlogin: () => void;
    ondraft: (value: string) => void;
    onsend: (original: boolean) => void;
  } = $props();
  let held = $state(false);
  let press: { id: number; started: number } | undefined;
  let holdTimer: number | undefined;
  let lastAttempt = -Infinity;
  const text = $derived(copy[language]);
  const invalidLine = $derived(/[\x00-\x1f\x7f]/.test(draft));
  const canSend = $derived(!!user && ready && !!draft.trim() && !busy && !invalidLine);

  function cancelPress() {
    clearTimeout(holdTimer);
    press = undefined;
    held = false;
  }
  onDestroy(cancelPress);
  $effect(() => { if (!canSend) cancelPress(); });

  function send(original: boolean) {
    const now = performance.now();
    if (!canSend || now - lastAttempt < 500) return;
    lastAttempt = now;
    cancelPress();
    onsend(original);
  }

  function pointerDown(event: PointerEvent) {
    if (event.button !== 0 || !event.isPrimary || !canSend) return;
    cancelPress();
    press = { id: event.pointerId, started: performance.now() };
    (event.currentTarget as HTMLButtonElement).setPointerCapture(event.pointerId);
    holdTimer = window.setTimeout(() => { held = true; }, 650);
  }

  function pointerUp(event: PointerEvent) {
    if (press?.id !== event.pointerId) return;
    const original = performance.now() - press.started >= 650;
    const bounds = (event.currentTarget as HTMLButtonElement).getBoundingClientRect();
    const inside = event.clientX >= bounds.left && event.clientX <= bounds.right && event.clientY >= bounds.top && event.clientY <= bounds.bottom;
    cancelPress();
    if (inside) send(original);
  }
</script>

<section class="composer" aria-label={text.compose}>
  {#if user}
    <div class="composer-heading">
      <label for="message-input">{text.compose} <span class="composer-nick">@{user.nick}</span></label>
      <span class="room-language">{languageName(room.language, language)}</span>
    </div>
    <textarea id="message-input" rows="2" value={draft} oninput={(event) => ondraft(event.currentTarget.value)} maxlength="4096" placeholder={`${room.name}…`} aria-invalid={invalidLine} aria-describedby="composer-help composer-error composer-readiness" onkeydown={(event) => {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
        event.preventDefault();
        if (!event.repeat) send(false);
      }
    }}></textarea>
    <div class="composer-actions">
      <div class="send-actions">
        <button type="button" class="text-button original-send" title={text.originalDescription} disabled={!canSend} onclick={() => send(true)}>{text.sendOriginal}</button>
        <button type="button" class="primary send-button" class:held disabled={!canSend} aria-describedby="composer-help" onpointerdown={pointerDown} onpointerup={pointerUp} onpointercancel={cancelPress} onlostpointercapture={cancelPress} oncontextmenu={(event) => event.preventDefault()} onclick={(event) => { if (event.detail === 0) send(false); }}>
          {busy ? text.sending : held ? text.releaseOriginal : text.send}
          <span aria-hidden="true">↑</span>
        </button>
      </div>
    </div>
    <p id="composer-readiness" class="composer-help" role="status">{!ready ? room.sendState === 'unavailable' ? text.unavailable : text.preparing : ''}</p>
    <p id="composer-help" class="composer-help">{text.holdHint}</p>
    <div id="composer-error" aria-live="polite">
      {#if invalidLine}<p class="error-text">{text.lineError}</p>{/if}
      {#if error}<p class="error-text">{error}</p><p class="field-note">{text.noRetry}</p>{/if}
    </div>
  {:else}
    <div class="guest-gate">
      <div><h2>{text.gateTitle}</h2><p>{text.gateDescription}</p></div>
      <button type="button" class="primary" onclick={onlogin}>{text.login} <span aria-hidden="true">→</span></button>
    </div>
  {/if}
</section>
