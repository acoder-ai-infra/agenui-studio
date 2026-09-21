'use client';

import { Suspense, useCallback, useEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { cn } from '@/lib/utils';
import {
  Database,
  Settings,
  ChevronDown,
  Plus,
  Sparkles,
  Zap,
  Boxes,
  Library,
  KeyRound,
  PackageOpen,
  HardDrive,
  type LucideIcon,
} from 'lucide-react';
import { useAdminConfig } from '../context';
import { useLocale, type MessageKey } from '@/lib/i18n/LocaleContext';
import { conversationApi, type ConversationSession } from '@/lib/generate-api';
import { RECENT_CHATS_CHANGED_EVENT } from '@/lib/recent-chats';

/**
 * 资源库：放置可复用、可引用、可版本化的生成资源。
 * 规则页为上传 Markdown 文档 + 异步解析后展示，不提供直接编辑创建。
 */
const resourceLibraryNavItems = [
  { icon: Database, labelKey: 'nav.datasources' as MessageKey, href: '/admin/datasources' },
  { icon: Zap,      labelKey: 'nav.operators' as MessageKey,   href: '/admin/operators' },
  { icon: Boxes,    labelKey: 'nav.rules' as MessageKey,       href: '/admin/rules' },
];

const systemNavItems = [
  { icon: KeyRound, labelKey: 'nav.modelProvider' as MessageKey, href: '/admin/settings' },
  { icon: PackageOpen, labelKey: 'nav.publication' as MessageKey, href: '/admin/settings/publication' },
];

/** 可折叠的导航分组，默认收起；父级只负责收放，不跳页面。 */
function NavGroup({
  icon: Icon,
  labelKey,
  items,
  defaultOpen = false,
}: {
  icon: LucideIcon;
  labelKey: MessageKey;
  items: { icon: LucideIcon; labelKey: MessageKey; href: string }[];
  defaultOpen?: boolean;
}) {
  const pathname = usePathname();
  const { t } = useLocale();
  const activeHref = items
    .filter((item) => pathname === item.href || pathname?.startsWith(item.href + '/'))
    .sort((left, right) => right.href.length - left.href.length)[0]?.href;
  const isChildActive = (href: string) => activeHref === href;
  const groupActive = Boolean(activeHref);
  // 当前页在本组里就先展开，否则刷新后会看不到自己在哪一项上
  const [open, setOpen] = useState(defaultOpen || groupActive);

  // 路由切进本组时自动展开。用「render 阶段比对上一次值」的写法而不是 effect：
  // 只在 groupActive 由假变真时推一次，用户手动收起当前所在的分组不会被立刻顶开。
  const [wasActive, setWasActive] = useState(groupActive);
  if (wasActive !== groupActive) {
    setWasActive(groupActive);
    if (groupActive) setOpen(true);
  }

  return (
    <div>
      <button
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        className={cn(
          'w-full flex items-center gap-3 px-3 py-2 rounded-lg text-[13px] font-medium transition-colors duration-150',
          groupActive ? 'text-neutral-900' : 'text-neutral-600 hover:bg-neutral-50 hover:text-neutral-900',
          // 收起时子项不可见，用底色兜住「当前在这个分组里」
          !open && groupActive && 'bg-neutral-100',
        )}
      >
        <Icon aria-hidden="true" className={cn('w-[18px] h-[18px] shrink-0', groupActive ? 'text-neutral-700' : 'text-neutral-400')} />
        <span className="flex-1 text-left">{t(labelKey)}</span>
        <ChevronDown aria-hidden="true" className={cn('w-3.5 h-3.5 text-neutral-400 transition-transform duration-200', !open && '-rotate-90')} />
      </button>
      {open && (
        <div className="mt-0.5 space-y-0.5">
          {items.map((item) => {
            const active = isChildActive(item.href);
            return (
              <Link
                key={item.href}
                href={item.href}
                className={cn(
                  // 子项只比父级多缩进 12px，图标尺寸与配色跟父级一致，
                  // 否则会出现一列小号浅灰图标，和父级图标错成两列
                  'flex items-center gap-3 pl-6 pr-3 py-1.5 rounded-lg text-[13px] transition-colors',
                  active
                    ? 'bg-neutral-100 text-neutral-900 font-medium'
                    : 'text-neutral-600 hover:bg-neutral-50 hover:text-neutral-900',
                )}
              >
                <item.icon aria-hidden="true" className={cn('w-[18px] h-[18px] shrink-0', active ? 'text-neutral-700' : 'text-neutral-400')} />
                {t(item.labelKey)}
              </Link>
            );
          })}
        </div>
      )}
    </div>
  );
}

function LocalRuntimeStatus() {
  const { t } = useLocale();
  return (
    <section aria-label={t('runtime.statusRegion')} className="shrink-0 border-t border-neutral-100 p-3">
      <div className="flex items-center gap-2.5 rounded-xl border border-neutral-200/70 bg-surface-raised px-3 py-2.5 shadow-sm">
        <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-neutral-100">
          <HardDrive aria-hidden="true" className="h-3.5 w-3.5 text-neutral-600" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5">
            <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-emerald-500" />
            <p className="truncate text-[11px] font-semibold text-neutral-700">{t('runtime.local')}</p>
          </div>
          <p className="mt-0.5 truncate text-[9px] text-neutral-500">{t('runtime.localHint')}</p>
        </div>
        <span className="shrink-0 rounded-full bg-emerald-50 px-1.5 py-0.5 text-[8px] font-medium text-emerald-700">{t('runtime.running')}</span>
      </div>
    </section>
  );
}

function RecentChats() {
  const [sessions, setSessions] = useState<ConversationSession[]>([]);
  const [expanded, setExpanded] = useState(true);
  const [loading, setLoading] = useState(true);
  const router = useRouter();
  const searchParams = useSearchParams();
  const activeSessionID = searchParams.get('session');
  const { currentTenantId, tenantsLoading } = useAdminConfig();
  const { t } = useLocale();
  const requestSequence = useRef(0);
  const lastAppliedRequest = useRef(0);

  const refreshSessions = useCallback(async (showLoading = false) => {
    if (!currentTenantId || tenantsLoading) return;
    const requestID = ++requestSequence.current;
    if (showLoading) setLoading(true);
    try {
      const res = await conversationApi.sessions({ tenantId: currentTenantId });
      if (requestID > lastAppliedRequest.current && res?.data?.list) {
        lastAppliedRequest.current = requestID;
        setSessions(res.data.list.slice(0, 20));
      }
    } catch {
      // Keep the last usable list when a background refresh fails.
    } finally {
      if (requestID === requestSequence.current) setLoading(false);
    }
  }, [currentTenantId, tenantsLoading]);

  useEffect(() => {
    void refreshSessions(true);
  }, [refreshSessions]);

  useEffect(() => {
    const refresh = () => void refreshSessions();
    window.addEventListener(RECENT_CHATS_CHANGED_EVENT, refresh);
    return () => window.removeEventListener(RECENT_CHATS_CHANGED_EVENT, refresh);
  }, [refreshSessions]);

  // Created/running/resuming can settle without another UI event (for example,
  // after a transient SSE disconnect). Poll only while automatic work is active.
  const shouldPoll = sessions.some((session) => (
    session.activeRunStatus === 'created' ||
    session.activeRunStatus === 'running' ||
    session.activeRunStatus === 'resuming'
  ));
  useEffect(() => {
    if (!shouldPoll) return;
    const timer = window.setInterval(() => void refreshSessions(), 5000);
    return () => window.clearInterval(timer);
  }, [refreshSessions, shouldPoll]);

  const getSessionTitle = (s: ConversationSession) => {
    if (s.title) return s.title;
    const meta = s.metadata as Record<string, unknown> | null;
    const metaTitle = meta?.title;
    if (typeof metaTitle === 'string' && metaTitle.trim()) {
      return metaTitle.replace(/^#\s*USER_REQUIREMENT\s*/i, '').trim();
    }
    const query = meta?.query;
    if (typeof query === 'string' && query.trim()) {
      return query.replace(/^#\s*USER_REQUIREMENT\s*/i, '').trim();
    }
    return t('chat.untitled', { id: s.sessionId.slice(0, 6) });
  };

  if (loading && sessions.length === 0) return null;

  return (
    <div className="mt-2 mx-3">
      <button
        onClick={() => setExpanded(!expanded)}
        className="w-full flex items-center gap-1 px-3 py-1.5 text-[12px] font-semibold text-neutral-400 hover:text-neutral-600 transition-colors"
      >
        <span className="flex-1 text-left">{t('chat.recent')}</span>
        <ChevronDown className={cn('w-3.5 h-3.5 transition-transform duration-200', !expanded && '-rotate-90')} />
      </button>
      {expanded && (
        <div className="space-y-0.5 max-h-[280px] overflow-y-auto mt-0.5">
          {sessions.map((s) => {
            const isActive = activeSessionID === s.sessionId;
            const isRunning = Boolean(s.activeRunStatus);
            return (
              <button
                key={s.sessionId}
                onClick={() =>
                  router.push(`/admin/home?session=${s.sessionId}`)
                }
                className={cn(
                  'w-full flex items-center gap-2.5 px-3 py-1.5 rounded-lg text-[12px] transition-colors text-left',
                  isActive
                    ? 'bg-neutral-100 text-neutral-900'
                    : 'text-neutral-500 hover:bg-neutral-50 hover:text-neutral-900',
                )}
              >
                <Sparkles className={cn('w-3.5 h-3.5 shrink-0', isActive ? 'text-neutral-600' : 'text-neutral-300')} />
                <span className="min-w-0 flex-1 truncate">{getSessionTitle(s)}</span>
                {isRunning && (
                  <span className="flex shrink-0 items-center gap-1 text-[9px] font-medium text-emerald-700">
                    <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-emerald-500 animate-pulse motion-reduce:animate-none" />
                    {t('chat.running')}
                  </span>
                )}
              </button>
            );
          })}
          {sessions.length === 0 && !loading && (
            <p className="px-3 py-2 text-[11px] text-neutral-400">{t('chat.empty')}</p>
          )}
        </div>
      )}
    </div>
  );
}

export function AdminSidebar() {
  const router = useRouter();
  const { t } = useLocale();

  return (
    <aside className="w-[220px] flex flex-col shrink-0 h-full bg-surface border-r border-neutral-200/70">
      {/* 新建聊天 */}
      <div className="shrink-0 px-3 pt-4 pb-3">
        <button
          onClick={() => router.push('/admin/home?new=1')}
          className="w-full flex items-center justify-center gap-2 h-9 rounded-lg border border-neutral-200 text-[13px] font-medium text-neutral-700 hover:bg-neutral-50 hover:border-neutral-300 transition-colors"
        >
          <Plus className="w-3.5 h-3.5" />
          {t('sidebar.newChat')}
        </button>
      </div>

      {/*
        开源版为单一本地工作区：不区分系统/普通空间，全部入口直接展示。
        中间导航区自己滚动，底部只展示本地运行状态，不提供不存在的空间切换能力。
      */}
      <div className="flex-1 min-h-0 overflow-y-auto pb-3">
        {/* 可复用资源、发布集成和系统配置分开组织，避免不同心智模型混在同一分组。 */}
        <div className="pt-3 border-t border-neutral-100 mx-3">
          <nav className="space-y-0.5">
            <NavGroup icon={Library} labelKey="sidebar.resourceLibrary" items={resourceLibraryNavItems} defaultOpen />
            <NavGroup icon={Settings} labelKey="sidebar.systemSettings" items={systemNavItems} />
          </nav>
        </div>

        {/* 最近聊天 */}
        <Suspense fallback={null}>
          <RecentChats />
        </Suspense>
      </div>

      <LocalRuntimeStatus />
    </aside>
  );
}
