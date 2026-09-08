<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import AuthDialog from '$lib/AuthDialog.svelte';
  import Composer from '$lib/Composer.svelte';
  import MessageFeed from '$lib/MessageFeed.svelte';
  import { APIError, errorText, mergeOutgoing, request, subscribeRoom, type Cursors, type Language, type Message, type Network, type Outgoing, type Room, type Session, type User } from '$lib/api';
  import { copy, languageName } from '$lib/i18n';

  let language = $state<Language>('en');
  let autoTranslate = $state(false);
  let user = $state<User | null>(null);
  let rooms = $state<Room[]>([]);
  let roomName = $state('');
  let network = $state<Network>({ state: 'connecting', detail: '' });
  let subscription = $state<'connecting' | 'live' | 'reconnecting'>('connecting');
  let initialized = $state(false);
  let restoring = $state(true);
  let sessionError = $state('');
  let actionError = $state('');
  let historyError = $state('');
  let sendsError = $state('');
  let historyLoading = $state(true);
  let messages = $state<Message[]>([]);
  let authMode = $state<'login' | 'register' | null>(null);
  let loggingOut = $state(false);
  let search = $state('');
  let showAll = $state(true);
  let favoritesOnly = $state(false);
  let favorites = $state<string[]>([]);
  let cursors = $state<Cursors>({});
  let storageError = $state(false);
  let drafts = $state<Record<string, string>>({});
  let outgoing = $state<Record<string, Outgoing>>({});
  let submitting = $state<Record<string, boolean>>({});
  let checking = $state<Record<string, boolean>>({});
  let sendErrors = $state<Record<string, { message: string; expiresAt: number }>>({});
  let now = $state(Date.now());
  let reconnectVersion = $state(0);
  let sessionController: AbortController | undefined;
  let accountController = new AbortController();
  let active: ReturnType<typeof subscribeRoom> | undefined;
  let accountEpoch = 0;
  let preferencesKey = '';
  let locallySubmitted = new Set<string>();
  const text = $derived(copy[language]);
  const room = $derived(rooms.find((candidate) => candidate.name === roomName));
  const ready = $derived(!!user && subscription === 'live' && room?.readState === 'ready' && room?.sendState === 'ready' && !historyLoading && !historyError);
  const visibleOutgoing = $derived(Object.values(outgoing).filter((send) => send.room === roomName && (send.state === 'confirmed' || Date.parse(send.expiresAt) > now)).sort((a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt)));
  const draft = $derived(drafts[roomName] ?? '');
  const busy = $derived(!!submitting[roomName] || visibleOutgoing.some((send) => send.original === draft && ['translating', 'sending', 'awaiting_echo'].includes(send.state)));
  const composeError = $derived((sendErrors[roomName]?.expiresAt ?? 0) > now ? sendErrors[roomName].message : '');
  const visibleRooms = $derived(rooms.filter((candidate, index) => {
    if (search.trim()) return candidate.name.toLowerCase().includes(search.trim().toLowerCase());
    if (favoritesOnly) return favorites.includes(candidate.name);
    return showAll || favorites.includes(candidate.name) || candidate.unreadCount > 0 || candidate.name === roomName || (favorites.length === 0 && index < 3);
  }).sort((a, b) => b.latestMessageId - a.latestMessageId));
  const connectionLabel = $derived(!room || historyLoading || room.readState === 'loading' ? text.loadingHistory
    : historyError || room.readState === 'unavailable' || room.sendState === 'unavailable' ? text.unavailable
    : subscription !== 'live' ? text[subscription]
    : user ? ready ? text.readyToChat : text.preparing : text.live);

  function storedPreferences(value: string | null): { favorites: string[]; cursors: Cursors; autoTranslate: boolean } {
    const result = { favorites: [] as string[], cursors: {} as Cursors, autoTranslate: false };
    if (!value) return result;
    const parsed = JSON.parse(value) as { favorites?: unknown; cursors?: unknown; autoTranslate?: unknown };
    result.autoTranslate = parsed.autoTranslate === true;
    const names = new Set(rooms.map((candidate) => candidate.name));
    if (Array.isArray(parsed.favorites)) result.favorites = parsed.favorites.filter((name): name is string => typeof name === 'string' && names.has(name));
    if (parsed.cursors && typeof parsed.cursors === 'object') {
      for (const [name, cursor] of Object.entries(parsed.cursors)) {
        if (names.has(name) && typeof cursor === 'number' && Number.isSafeInteger(cursor) && cursor >= 0) result.cursors[name] = cursor;
      }
    }
    return result;
  }

  function persistPreferences() {
    if (!preferencesKey) return;
    try {
      const stored = storedPreferences(localStorage.getItem(preferencesKey));
      const merged = { ...cursors };
      for (const [name, cursor] of Object.entries(stored.cursors)) {
        if (cursor > (merged[name] ?? 0)) { merged[name] = cursor; active?.read(name, cursor); }
      }
      cursors = merged;
      localStorage.setItem(preferencesKey, JSON.stringify({ favorites, cursors, autoTranslate }));
      storageError = false;
    } catch { storageError = true; }
  }

  function switchAccount(next: User | null) {
    if (preferencesKey && user?.id === next?.id) { user = next; return; }
    active?.stop();
    active = undefined;
    accountController.abort();
    accountController = new AbortController();
    accountEpoch++;
    user = next;
    messages = [];
    outgoing = {};
    drafts = {};
    sendErrors = {};
    submitting = {};
    checking = {};
    locallySubmitted = new Set();
    subscription = 'connecting';
    historyLoading = true;
    historyError = sendsError = '';
    search = '';
    showAll = true;
    favoritesOnly = false;
    preferencesKey = `autoirc2p:rooms:v1:${next?.id ?? 'guest'}`;
    favorites = [];
    cursors = {};
    autoTranslate = false;
    try {
      const stored = storedPreferences(localStorage.getItem(preferencesKey));
      favorites = stored.favorites;
      cursors = stored.cursors;
      autoTranslate = stored.autoTranslate;
      storageError = false;
    } catch { storageError = true; }
    roomName = favorites[0] ?? rooms[0]?.name ?? '';
    reconnectVersion++;
  }

  function receiveRooms(next: Room[], replace: boolean) {
    const mergedCursors = { ...cursors };
    let baselineChanged = false;
    for (const candidate of next) {
      if (!(candidate.name in mergedCursors)) {
        mergedCursors[candidate.name] = candidate.latestMessageId;
        baselineChanged = true;
      }
    }
    if (baselineChanged) { cursors = mergedCursors; persistPreferences(); }
    const normalized = next.map((candidate) => candidate.latestMessageId <= (cursors[candidate.name] ?? -1) ? { ...candidate, unreadCount: 0 } : candidate);
    rooms = replace ? normalized : rooms.map((candidate) => normalized.find((incoming) => incoming.name === candidate.name) ?? candidate);
  }

  function markRead(selectedRoom: string, messageId: number) {
    if (selectedRoom !== roomName || historyLoading || historyError || subscription !== 'live' || document.visibilityState !== 'visible' || !document.hasFocus() || messageId <= (cursors[roomName] ?? 0)) return;
    cursors = { ...cursors, [roomName]: messageId };
    rooms = rooms.map((candidate) => candidate.name === selectedRoom && candidate.unreadCount > 0 && candidate.latestMessageId <= messageId ? { ...candidate, unreadCount: 0 } : candidate);
    persistPreferences();
    active?.read(roomName, cursors[roomName]);
  }

  function receiveSends(sends: Outgoing[]) {
    now = Date.now();
    const next = { ...outgoing };
    let refreshConfirmed = false;
    for (const incoming of sends) {
      const previous = next[incoming.requestId];
      const send = mergeOutgoing(previous, incoming);
      next[send.requestId] = send;
      if (send.state === 'confirmed' && previous?.state !== 'confirmed') {
        if (locallySubmitted.delete(send.requestId) && drafts[send.room] === send.original) drafts = { ...drafts, [send.room]: '' };
        if (send.room === roomName) refreshConfirmed = true;
      }
    }
    outgoing = next;
    if (refreshConfirmed) active?.refresh();
  }

  function refreshAccount() {
    initialized = false;
    switchAccount(null);
    void restoreSession();
  }

  function notifyAccountChange() {
    try { localStorage.setItem('autoirc2p:session-change', crypto.randomUUID()); }
    catch { storageError = true; }
  }

  async function restoreSession() {
    sessionController?.abort();
    sessionController = new AbortController();
    const signal = sessionController.signal;
    restoring = true;
    sessionError = '';
    try {
      const session = await request<Session>('/api/session', { signal });
      if (signal.aborted) return;
      rooms = session.rooms;
      network = session.network;
      switchAccount(session.user);
      const snapshot = await request<{ rooms: Room[] }>(`/api/rooms?${new URLSearchParams({ cursors: JSON.stringify(cursors) })}`, { signal });
      if (signal.aborted) return;
      receiveRooms(snapshot.rooms, true);
      if (!rooms.some((candidate) => candidate.name === roomName)) roomName = rooms[0]?.name ?? '';
      initialized = true;
      reconnectVersion++;
    } catch (error) {
      if (!signal.aborted) sessionError = errorText(error);
    } finally {
      if (!signal.aborted) restoring = false;
    }
  }

  onMount(() => {
    language = navigator.language.toLowerCase().startsWith('en') ? 'en' : 'ko';
    void restoreSession();
    const storageChanged = (event: StorageEvent) => {
      if (event.key === 'autoirc2p:session-change') { refreshAccount(); return; }
      if (event.key !== preferencesKey || !event.newValue) return;
      try {
        const stored = storedPreferences(event.newValue);
        favorites = stored.favorites;
        autoTranslate = stored.autoTranslate;
        const merged = { ...cursors };
        for (const [name, cursor] of Object.entries(stored.cursors)) {
          if (cursor > (merged[name] ?? 0)) { merged[name] = cursor; active?.read(name, cursor); }
        }
        cursors = merged;
      } catch { storageError = true; }
    };
    window.addEventListener('storage', storageChanged);
    return () => {
      sessionController?.abort();
      accountController.abort();
      active?.stop();
      window.removeEventListener('storage', storageChanged);
    };
  });

  $effect(() => { document.documentElement.lang = language; });

  $effect(() => {
    const expiries = [...Object.values(outgoing).filter((send) => send.state !== 'confirmed').map((send) => Date.parse(send.expiresAt)), ...Object.values(sendErrors).map((error) => error.expiresAt)].filter((expiry) => expiry > now);
    if (expiries.length === 0) return;
    const timer = window.setTimeout(() => { now = Date.now(); }, Math.max(1, Math.min(...expiries) - Date.now()));
    return () => clearTimeout(timer);
  });

  $effect(() => {
    if (!initialized || !roomName) return;
    const selectedRoom = roomName;
    const selectedLanguage = language;
    const selectedAutoTranslate = autoTranslate;
    const selectedUserId = user?.id ?? 0;
    reconnectVersion;
    return untrack(() => {
      messages = [];
      historyLoading = true;
      historyError = sendsError = '';
      subscription = 'connecting';
      const next = subscribeRoom({
        room: selectedRoom,
        language: selectedLanguage,
        autoTranslate: selectedAutoTranslate,
        userId: selectedUserId,
        cursors: () => cursors,
        onIdentityMismatch: refreshAccount,
        onMessages: (incoming) => {
          messages = incoming;
          for (const message of incoming) {
            if (!message.own || !message.requestId || !outgoing[message.requestId]) continue;
            const send = outgoing[message.requestId];
            if (send.state !== 'confirmed') receiveSends([{ ...send, state: 'confirmed', messageId: message.id }]);
          }
        },
        onRooms: receiveRooms,
        onSends: receiveSends,
        onSendsError: (error) => { sendsError = error; },
        onNetwork: (incoming) => { network = incoming; },
        onSubscription: (incoming) => { subscription = incoming; },
        onHistory: (loading, error) => { historyLoading = loading; historyError = error; }
      });
      active = next;
      return () => { next.stop(); if (active === next) active = undefined; };
    });
  });

  function sendFailureText(error: unknown): string {
    if (!(error instanceof APIError)) return text.notSent;
    switch (error.code) {
      case 'not_ready': return text.preparing;
      case 'translation_failed': return text.translationSendFailed;
      case 'invalid_message': return text.messageInvalid;
      case 'request_conflict': return text.requestConflict;
      case 'login_required': return text.signInToSend;
      default: return text.notSent;
    }
  }

  function setSendError(selectedRoom: string, message: string) {
    now = Date.now();
    sendErrors = { ...sendErrors, [selectedRoom]: { message, expiresAt: now + 10000 } };
  }

  async function checkSend(send: Outgoing) {
    if (checking[send.requestId]) return;
    const epoch = accountEpoch;
    checking = { ...checking, [send.requestId]: true };
    try {
      const result = await request<{ send: Outgoing }>(`/api/sends/${encodeURIComponent(send.requestId)}`, { signal: accountController.signal });
      if (epoch === accountEpoch) receiveSends([result.send]);
    } catch (error) {
      if (epoch === accountEpoch) setSendError(send.room, sendFailureText(error));
    } finally {
      if (epoch === accountEpoch) checking = { ...checking, [send.requestId]: false };
    }
  }

  async function sendMessage(original: boolean) {
    if (!room || !ready || busy || !draft.trim()) return;
    const selectedRoom = room.name;
    const content = draft;
    const epoch = accountEpoch;
    const requestId = crypto.randomUUID();
    const started = Date.now();
    const provisional: Outgoing = { requestId, room: selectedRoom, original: content, state: 'unconfirmed', messageId: 0, createdAt: new Date(started).toISOString(), expiresAt: new Date(started + 120000).toISOString(), errorCode: 'send_unconfirmed' };
    locallySubmitted.add(requestId);
    submitting = { ...submitting, [selectedRoom]: true };
    sendErrors = { ...sendErrors, [selectedRoom]: { message: '', expiresAt: 0 } };
    try {
      const result = await request<{ send: Outgoing }>('/api/messages', {
        method: 'POST', signal: AbortSignal.any([accountController.signal, AbortSignal.timeout(120000)]),
        body: JSON.stringify({ room: selectedRoom, text: content, original: original || !autoTranslate, requestId })
      });
      if (epoch === accountEpoch) receiveSends([result.send]);
    } catch (cause) {
      if (epoch !== accountEpoch) return;
      submitting = { ...submitting, [selectedRoom]: false };
      if (cause instanceof APIError && cause.send) {
        receiveSends([cause.send]);
      } else if (cause instanceof APIError && cause.status >= 400 && cause.status < 500) {
        receiveSends([{ ...provisional, state: 'failed', errorCode: cause.code }]);
      } else {
        // A lost response cannot tell us whether the server wrote the message.
        // Status recovery is a GET; the original POST is never replayed.
        try {
          const result = await request<{ send: Outgoing }>(`/api/sends/${encodeURIComponent(requestId)}`, { signal: AbortSignal.any([accountController.signal, AbortSignal.timeout(10000)]) });
          if (epoch === accountEpoch) receiveSends([result.send]);
        } catch {
          if (epoch === accountEpoch && !outgoing[requestId]) receiveSends([provisional]);
        }
      }
      if (epoch !== accountEpoch) return;
      if (outgoing[requestId]?.state !== 'confirmed') setSendError(selectedRoom, sendFailureText(cause));
      if (cause instanceof APIError && cause.status === 401) { switchAccount(null); authMode = 'login'; }
    } finally {
      if (epoch === accountEpoch) submitting = { ...submitting, [selectedRoom]: false };
    }
  }

  async function logout() {
    if (loggingOut) return;
    loggingOut = true;
    actionError = '';
    try {
      await request<void>('/api/auth/logout', { method: 'POST' });
      notifyAccountChange();
      switchAccount(null);
      await restoreSession();
    } catch (error) { actionError = errorText(error); }
    finally { loggingOut = false; }
  }

  function authenticated(next: User) {
    switchAccount(next);
    authMode = null;
    notifyAccountChange();
    void restoreSession();
  }

  function toggleFavorite(name: string) {
    favorites = favorites.includes(name) ? favorites.filter((favorite) => favorite !== name) : [...favorites, name];
    persistPreferences();
  }
</script>

<svelte:head>
  <title>{roomName ? `${roomName} — ` : ''}AutoIRC2P</title>
  <meta name="description" content="Conversations with translations and original messages side by side." />
</svelte:head>

<a class="skip-link" href="#conversation">{text.messages}</a>
<div class="workspace">
  <aside class="sidebar" aria-label={text.rooms}>
    <a class="brand" href="/" aria-label="AutoIRC2P"><span class="brand-symbol" aria-hidden="true">a/</span><span>AutoIRC<span class="brand-suffix">2P</span></span></a>
    <div class="sidebar-section room-navigation">
      <h2>{text.rooms}</h2>
      <label class="room-search"><span class="sr-only">{text.searchRooms}</span><input type="search" bind:value={search} placeholder={text.searchRooms} /></label>
      <div class="room-filters">
        <button type="button" class="text-button" aria-pressed={favoritesOnly} onclick={() => { favoritesOnly = !favoritesOnly; showAll = false; }}>{text.favorites}</button>
        <button type="button" class="text-button" aria-expanded={showAll} onclick={() => { showAll = !showAll; favoritesOnly = false; }}>{showAll ? text.focusedRooms : text.allRooms}</button>
      </div>
      <nav aria-label={text.rooms}>
        {#each visibleRooms as candidate (candidate.name)}
          <div class="room-row" class:selected={candidate.name === roomName}>
            <button type="button" class="room-button" class:selected={candidate.name === roomName} aria-current={candidate.name === roomName ? 'page' : undefined} onclick={() => { roomName = candidate.name; }}>
              <span class="room-name">{candidate.name}</span>
              {#if candidate.unreadCount > 0}<span class="unread-badge" aria-label={`${candidate.unreadCount} ${text.unread}`}>{candidate.unreadCount > 99 ? '99+' : candidate.unreadCount}</span>{:else}<span class="room-code" title={`${text.outgoingLanguage}: ${languageName(candidate.language, language)}`}>{candidate.language.toUpperCase()}</span>{/if}
            </button>
            <button type="button" class="favorite-button" aria-pressed={favorites.includes(candidate.name)} aria-label={`${favorites.includes(candidate.name) ? text.removeFavorite : text.addFavorite}: ${candidate.name}`} onclick={() => toggleFavorite(candidate.name)}>
              <svg viewBox="0 0 20 20" aria-hidden="true"><path d="m10 2 2.5 5.1 5.6.8-4 3.9.9 5.5-5-2.6-5 2.6.9-5.5-4-3.9 5.6-.8Z" /></svg>
            </button>
          </div>
        {/each}
      </nav>
      {#if search && visibleRooms.length === 0}<p class="field-note">{text.noResults}</p><button type="button" class="text-button" onclick={() => { search = ''; }}>{text.clearSearch}</button>
      {:else if favoritesOnly && favorites.length === 0}<p class="field-note">{text.noFavorites}</p>
      {:else if initialized && rooms.length === 0}<p class="field-note">{text.noRooms}</p>{/if}
      {#if storageError}<p class="field-note" role="status">{text.storageUnavailable}</p>{/if}
    </div>
    <details class="network-panel">
      <summary>{text.network}</summary>
      <div class="network-state"><span>{network.state}</span></div>
      <p class="network-detail">{network.detail || text.connectionNote}</p>
      <button type="button" class="text-button refresh-button" disabled={restoring} onclick={() => void restoreSession()}>{text.refreshSession}</button>
    </details>
  </aside>

  <main id="conversation" class="conversation" tabindex="-1">
    <header class="workspace-header">
      <div class="room-heading"><span class="context-label">{text.selectedRoom}</span><h1>{roomName || 'AutoIRC2P'}</h1></div>
      <div class="header-controls">
        <button type="button" class="account-button" role="switch" aria-checked={autoTranslate} aria-label={text.autoTranslate} onclick={() => { autoTranslate = !autoTranslate; persistPreferences(); }}>{text.autoTranslate} {autoTranslate ? 'On' : 'Off'}</button>
        <label class="language-control"><span>{autoTranslate ? text.displayLanguage : text.interfaceLanguage}</span><select bind:value={language} aria-label={autoTranslate ? text.displayLanguage : text.interfaceLanguage}><option value="en">English</option><option value="ko">한국어</option></select></label>
        {#if user}<button type="button" class="account-button" disabled={loggingOut} onclick={() => void logout()} title={`@${user.nick} · ${text.logout}`}><span class="account-nick">@{user.nick}</span><span>{loggingOut ? text.working : text.logout}</span></button>
        {:else}<button type="button" class="account-button" onclick={() => { authMode = 'login'; }}>{text.login}</button>{/if}
      </div>
    </header>
    <div class="conversation-status" aria-live="polite">
      <span class="feed-status"><span class="status-dot" class:ready={ready || (!user && subscription === 'live' && room?.readState === 'ready')} aria-hidden="true"></span>{connectionLabel}</span>
      {#if room}<span>{text.outgoingLanguage} <strong>{languageName(room.language, language)}</strong></span>{/if}
      {#if subscription === 'reconnecting' || room?.sendState === 'unavailable' || room?.readState === 'unavailable'}<button type="button" class="text-button connection-retry" disabled={restoring} onclick={() => void restoreSession()}>{text.retry}</button>{/if}
      <span class="read-mode">{user ? `@${user.nick}` : text.guest}</span>
    </div>
    {#if actionError}<div class="workspace-alert" role="alert">{actionError}</div>{/if}
    {#if sessionError}<div class="workspace-alert" role="alert"><div><strong>{text.sessionError}</strong><p>{sessionError}</p></div><button type="button" onclick={() => void restoreSession()} disabled={restoring}>{text.retry}</button></div>{/if}
    {#if !initialized}
      <div class="workspace-loading" role="status"><span class="empty-mark" aria-hidden="true">a/</span><p>{restoring ? text.initializing : text.sessionError}</p></div>
    {:else if room}
      {#key `${roomName}:${language}:${user?.id ?? 'guest'}`}
        <MessageFeed {language} {autoTranslate} {roomName} {messages} outgoing={visibleOutgoing} loading={historyLoading} error={historyError} {sendsError} {checking} onread={markRead} onretry={() => active?.refresh()} oncheck={(send) => void checkSend(send)} onrestore={(send) => { drafts = { ...drafts, [send.room]: send.original }; }} />
      {/key}
      {#key `${roomName}:${user?.id ?? 'guest'}`}
        <Composer {language} {autoTranslate} {room} {user} {draft} {ready} {busy} error={composeError} onlogin={() => { authMode = 'login'; }} ondraft={(value) => { drafts = { ...drafts, [roomName]: value }; }} onsend={(original) => void sendMessage(original)} />
      {/key}
    {:else}<div class="workspace-loading"><p>{text.noRooms}</p></div>{/if}
  </main>
</div>

{#if authMode}<AuthDialog {language} mode={authMode} onclose={() => { authMode = null; }} onauthenticated={authenticated} />{/if}
