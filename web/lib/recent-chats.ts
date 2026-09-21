export const RECENT_CHATS_CHANGED_EVENT = 'agenui:recent-chats-changed';

export interface RecentChatsChangedDetail {
  sessionId?: string;
}

/** Notify the persistent admin sidebar after a chat Run is created or settles. */
export function notifyRecentChatsChanged(sessionId?: string): void {
  if (typeof window === 'undefined') return;
  window.dispatchEvent(new CustomEvent<RecentChatsChangedDetail>(RECENT_CHATS_CHANGED_EVENT, {
    detail: { sessionId },
  }));
}
