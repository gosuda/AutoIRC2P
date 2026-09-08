<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import { errorText, passwordHash, request, type Language, type User } from './api';
  import { copy } from './i18n';

  let { language, mode = 'login', onclose, onauthenticated }: {
    language: Language;
    mode: 'login' | 'register';
    onclose: () => void;
    onauthenticated: (user: User) => void;
  } = $props();
  let dialog: HTMLDialogElement;
  let password = $state('');
  let nick = $state('');
  let busy = $state(false);
  let error = $state('');
  let controller: AbortController | undefined;
  const text = $derived(copy[language]);

  $effect(() => {
    dialog.showModal();
    return () => dialog.close();
  });
  onDestroy(() => {
    controller?.abort();
    password = '';
  });

  function close() {
    controller?.abort();
    password = '';
    onclose();
  }

  async function authenticate(event: SubmitEvent) {
    event.preventDefault();
    if (busy) return;
    busy = true;
    error = '';
    controller = new AbortController();
    const signal = controller.signal;
    const operation = mode;
    const nickname = nick.trim();
    const derivation = passwordHash(nickname, password, signal);
    password = '';
    let hash = '';
    try {
      hash = await derivation;
      signal.throwIfAborted();
      const result = await request<{ user: User }>(`/api/auth/${operation}`, {
        method: 'POST', signal,
        body: JSON.stringify({ nick: nickname, passwordHash: hash })
      });
      if (!signal.aborted) onauthenticated(result.user);
    } catch (cause) {
      if (!signal.aborted) {
        error = errorText(cause);
        await tick();
        dialog.querySelector<HTMLInputElement>('#auth-password')?.focus();
      }
    } finally {
      hash = '';
      password = '';
      busy = false;
    }
  }
</script>

<dialog bind:this={dialog} class="auth-dialog" aria-labelledby="auth-title" aria-describedby="auth-description" oncancel={(event) => { event.preventDefault(); close(); }}>
  <div class="dialog-topline">
    <span class="brand-small">AutoIRC2P</span>
    <button type="button" class="text-button" onclick={close}>{text.close} <span aria-hidden="true">×</span></button>
  </div>
  <h2 id="auth-title">{mode === 'register' ? text.registerTitle : text.loginTitle}</h2>
  <p id="auth-description" class="muted">{mode === 'register' ? text.registerDescription : text.loginDescription}</p>
  <form onsubmit={authenticate} aria-busy={busy}>
    <label for="auth-nick">{text.nick}</label>
    <input id="auth-nick" name="nick" autocomplete="username" autocapitalize="none" spellcheck="false" bind:value={nick} required minlength="3" maxlength="24" pattern={'[A-Za-z][A-Za-z0-9_\\-]{2,23}'} aria-describedby="nick-note" />
    <p class="field-note" id="nick-note">{text.nickNote}</p>
    <label for="auth-password">{text.password}</label>
    <input id="auth-password" type="password" name="password" autocomplete={mode === 'register' ? 'new-password' : 'current-password'} bind:value={password} required minlength={mode === 'register' ? 12 : 1} maxlength="1024" aria-describedby={error ? 'auth-error' : 'password-note'} aria-invalid={!!error} />
    <p id="password-note" class="field-note">{text.passwordNote}</p>
    {#if error}<p id="auth-error" class="error-text" role="alert">{error}</p>{/if}
    <button type="submit" class="primary auth-submit" disabled={busy}>{busy ? text.authenticating : mode === 'register' ? text.register : text.login}</button>
  </form>
  <div class="auth-switch">
    <span class="muted">{mode === 'register' ? text.haveAccount : text.needAccount}</span>
    <button type="button" class="text-button" disabled={busy} onclick={() => { mode = mode === 'register' ? 'login' : 'register'; password = ''; error = ''; }}>{mode === 'register' ? text.login : text.register}</button>
  </div>
</dialog>
