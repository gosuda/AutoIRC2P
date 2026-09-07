export type Language = 'en' | 'ko';
export type User = { id: number; email: string; nick: string };
export type Room = {
  name: string;
  language: string;
  latestMessageId: number;
  unreadCount: number;
  readState: 'loading' | 'ready' | 'unavailable';
  sendState: 'login_required' | 'preparing' | 'ready' | 'unavailable';
};
export type Outgoing = {
  requestId: string;
  room: string;
  original: string;
  state: 'translating' | 'sending' | 'awaiting_echo' | 'confirmed' | 'failed' | 'unconfirmed';
  messageId: number;
  createdAt: string;
  expiresAt: string;
  errorCode: string;
};
export type Cursors = Record<string, number>;
export type Network = { state: string; detail: string };
export type Session = {
  user: User | null;
  rooms: Room[];
  network: Network;
  displayLanguage: Language;
};
export type Message = {
  id: number;
  room: string;
  roomLanguage?: string;
  nick: string;
  original: string;
  sourceLanguage: string;
  translation: string;
  targetLanguage: string;
  translationState: 'pending' | 'ready' | 'failed' | 'excluded';
  createdAt: string;
  service: boolean;
  own: boolean;
  requestId?: string;
};
type Frame = { type: 'message' | 'translation'; message: Message }
  | ({ type: 'status' } & Network)
  | { type: 'identity'; userId: number }
  | { type: 'rooms'; rooms: Room[] }
  | { type: 'room'; room: Room }
  | { type: 'send'; send: Outgoing };

export class APIError extends Error {
  constructor(message: string, public readonly status: number, public readonly code = '', public readonly send?: Outgoing) {
    super(message);
    this.name = 'APIError';
  }
}

export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers);
  if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
  const response = await fetch(path, {
    ...options,
    credentials: 'same-origin',
    cache: 'no-store',
    headers
  });
  const body = await response.text();
  let data: unknown;
  try {
    data = body ? JSON.parse(body) : undefined;
  } catch {
    throw new APIError(`Unexpected server response (${response.status}).`, response.status);
  }
  if (!response.ok) {
    const message = data && typeof data === 'object' && 'error' in data && typeof data.error === 'string'
      ? data.error : `Request failed (${response.status}).`;
    const failure = data as { code?: string; send?: Outgoing } | undefined;
    throw new APIError(message, response.status, failure?.code, failure?.send);
  }
  return data as T;
}

export function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export async function passwordHash(email: string, password: string, signal: AbortSignal): Promise<string> {
  if (!globalThis.crypto?.subtle) throw new Error('A secure HTTPS connection or localhost is required to sign in.');
  const parameters = await request<{ salt: string; iterations: number; algorithm: string }>(
    `/api/auth/salt?email=${encodeURIComponent(email)}`, { signal }
  );
  if (parameters.algorithm !== 'PBKDF2-SHA256' || parameters.iterations !== 600000 ||
      !/^(?:[a-f\d]{2})+$/i.test(parameters.salt)) {
    throw new Error('The server returned unsupported password parameters.');
  }
  const salt = Uint8Array.from(parameters.salt.match(/../g)!, (byte) => parseInt(byte, 16));
  const bytes = new TextEncoder().encode(password);
  password = '';
  try {
    const key = await crypto.subtle.importKey('raw', bytes, 'PBKDF2', false, ['deriveBits']);
    const result = new Uint8Array(await crypto.subtle.deriveBits(
      { name: 'PBKDF2', hash: 'SHA-256', salt, iterations: parameters.iterations }, key, 256
    ));
    try {
      signal.throwIfAborted();
      return Array.from(result, (byte) => byte.toString(16).padStart(2, '0')).join('');
    } finally {
      result.fill(0);
    }
  } finally {
    bytes.fill(0);
  }
}

export function messageKey(message: Message): number | string {
  return message.id || `service:${message.room}:${message.createdAt}:${message.nick}:${message.original}`;
}

export function mergeOutgoing(previous: Outgoing | undefined, incoming: Outgoing): Outgoing {
  if (!previous) return incoming;
  const rank: Record<Outgoing['state'], number> = { translating: 0, sending: 1, awaiting_echo: 2, failed: 3, unconfirmed: 3, confirmed: 4 };
  return rank[incoming.state] < rank[previous.state] ? previous : incoming;
}

export function subscribeRoom(options: {
  room: string;
  language: Language;
  autoTranslate: boolean;
  userId: number;
  cursors: () => Cursors;
  onMessages: (messages: Message[]) => void;
  onRooms: (rooms: Room[], replace: boolean) => void;
  onSends: (sends: Outgoing[]) => void;
  onSendsError: (error: string) => void;
  onNetwork: (network: Network) => void;
  onSubscription: (state: 'connecting' | 'live' | 'reconnecting') => void;
  onHistory: (loading: boolean, error: string) => void;
  onIdentityMismatch: () => void;
}) {
  const query = new URLSearchParams({ room: options.room, lang: options.autoTranslate ? options.language : 'original' });
  const controller = new AbortController();
  let socket: WebSocket | undefined;
  let verifiedSocket: WebSocket | undefined;
  let retryTimer: number | undefined;
  let readTimer: number | undefined;
  const pendingReads = new Map<string, number>();
  let stopped = false;
  let retries = 0;
  let historyGeneration = 0;
  let historyPending = false;
  let messages = new Map<number | string, Message>();
  let duringHistory = new Map<number | string, Message>();

  function publish() {
    const visible = [...messages.values()].filter((message) => {
      const nick = message.nick.toLowerCase();
      return nick !== 'nickserv' && nick !== 'chanserv';
    });
    options.onMessages(visible.sort((a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt) || a.id - b.id));
  }

  function mergeMessage(previous: Message | undefined, incoming: Message): Message {
    if (previous?.translationState === 'ready' && incoming.translationState === 'pending' && previous.targetLanguage === incoming.targetLanguage) {
      return { ...incoming, translation: previous.translation, translationState: 'ready' };
    }
    return incoming;
  }

  async function refreshHistory() {
    const generation = ++historyGeneration;
    historyPending = true;
    duringHistory = new Map();
    options.onHistory(true, '');
    try {
      const data = await request<{ messages: Message[] }>(`/api/messages?${query}`, { signal: controller.signal });
      if (stopped || generation !== historyGeneration) return;
      const history = new Map(messages);
      for (const message of data.messages) {
        const key = messageKey(message);
        history.set(key, mergeMessage(history.get(key), message));
      }
      for (const [key, message] of duringHistory) history.set(key, mergeMessage(history.get(key), message));
      messages = history;
      publish();
      options.onHistory(false, '');
    } catch (error) {
      if (!stopped && generation === historyGeneration) options.onHistory(false, errorText(error));
    } finally {
      if (generation === historyGeneration) {
        historyPending = false;
        duringHistory.clear();
      }
    }
  }

  async function refreshSends() {
    if (!options.userId) return;
    try {
      const data = await request<{ sends: Outgoing[] }>(`/api/sends?room=${encodeURIComponent(options.room)}`, { signal: controller.signal });
      if (!stopped) { options.onSends(data.sends); options.onSendsError(''); }
    } catch (error) {
      if (!stopped) options.onSendsError(errorText(error));
    }
  }

  function flushRead() {
    readTimer = undefined;
    if (stopped || socket?.readyState !== WebSocket.OPEN || socket !== verifiedSocket) {
      pendingReads.clear();
      return;
    }
    const next = pendingReads.entries().next().value;
    if (next) {
      const [room, messageId] = next;
      pendingReads.delete(room);
      socket.send(JSON.stringify({ type: 'read', room, messageId }));
    }
    if (pendingReads.size > 0) readTimer = window.setTimeout(flushRead, 500);
  }

  function read(room: string, messageId: number) {
    if (socket?.readyState !== WebSocket.OPEN || socket !== verifiedSocket) return;
    pendingReads.set(room, Math.max(messageId, pendingReads.get(room) ?? 0));
    if (readTimer === undefined) readTimer = window.setTimeout(flushRead, 500);
  }

  function connect() {
    if (stopped) return;
    options.onSubscription(retries ? 'reconnecting' : 'connecting');
    const socketQuery = new URLSearchParams(query);
    socketQuery.set('cursors', JSON.stringify(options.cursors()));
    const url = new URL(`/api/ws?${socketQuery}`, location.href);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const current = new WebSocket(url);
    socket = current;
    current.onmessage = (event) => {
      if (stopped || socket !== current) return;
      let frame: Frame;
      try { frame = JSON.parse(event.data) as Frame; }
      catch { current.close(); return; }
      if (frame.type === 'identity') {
        if (frame.userId !== options.userId) {
          current.close();
          options.onIdentityMismatch();
          return;
        }
        verifiedSocket = current;
        void refreshHistory();
        void refreshSends();
        return;
      }
      if (verifiedSocket !== current) return;
      if (frame.type === 'rooms') {
        options.onRooms(frame.rooms, true);
        retries = 0;
        options.onSubscription('live');
      } else if (frame.type === 'room') {
        options.onRooms([frame.room], false);
      } else if (frame.type === 'send') {
        options.onSends([frame.send]);
      } else if (frame.type === 'status') {
        options.onNetwork({ state: frame.state, detail: frame.detail });
      } else if ((frame.type === 'message' || frame.type === 'translation') && frame.message?.room === options.room) {
        const key = messageKey(frame.message);
        const next = mergeMessage(messages.get(key), frame.message);
        messages.set(key, next);
        if (historyPending) duringHistory.set(key, next);
        publish();
      }
    };
    current.onerror = () => current.close();
    current.onclose = () => {
      if (stopped || socket !== current) return;
      verifiedSocket = undefined;
      clearTimeout(readTimer);
      readTimer = undefined;
      pendingReads.clear();
      options.onSubscription('reconnecting');
      retryTimer = window.setTimeout(connect, Math.min(30000, 1000 * 2 ** Math.min(retries++, 5)));
    };
  }

  connect();
  return {
    read,
    refresh: () => { if (socket && socket === verifiedSocket) { void refreshHistory(); void refreshSends(); } },
    stop: () => {
      stopped = true;
      controller.abort();
      clearTimeout(retryTimer);
      clearTimeout(readTimer);
      pendingReads.clear();
      if (socket) {
        socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null;
        socket.close();
      }
    }
  };
}
