'use client';

import { useState, useEffect, useRef } from 'react';
import Image from 'next/image';
import Link from 'next/link';
import { LogIn, Menu, X } from 'lucide-react';
import { useAuth, redirectToLogin } from '../lib/auth';
import { useAdminConfig } from '../context';
import { useLocale } from '@/lib/i18n/LocaleContext';
import { LocaleToggle } from './LocaleToggle';
import { ThemeToggle } from './ThemeToggle';

function UserMenu() {
  const { user, loading } = useAuth();
  const { t } = useLocale();
  const [open, setOpen] = useState(false);
  const [avatarErrored, setAvatarErrored] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  if (loading) {
    return <div aria-hidden="true" className="h-7 w-20 animate-pulse rounded-lg bg-neutral-100 motion-reduce:animate-none" />;
  }

  if (!user) {
    return (
      <button
        type="button"
        onClick={() => redirectToLogin()}
        className="inline-flex h-7 items-center gap-1.5 rounded-lg px-2 text-xs font-medium text-neutral-600 transition-colors hover:bg-neutral-100 hover:text-neutral-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
      >
        <LogIn aria-hidden="true" className="h-3.5 w-3.5" />
        {t('auth.signIn')}
      </button>
    );
  }

  const displayName = user.DisplayName || user.nickNameCn || user.lastName || user.loginName || '';
  const avatarUrl = user.PicURL || user.avatarURL || '';
  const showImg = !!avatarUrl && !avatarErrored;

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        aria-haspopup="menu"
        aria-label={t('nav.accountMenu')}
        className="flex h-7 min-w-0 items-center gap-2 rounded-lg px-1.5 transition-colors hover:bg-neutral-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
      >
        <span className="max-w-28 truncate text-[12px] font-medium text-neutral-700">{displayName}</span>
        {showImg ? (
          // eslint-disable-next-line @next/next/no-img-element
          <img
            src={avatarUrl}
            alt={displayName}
            width={24}
            height={24}
            onError={() => setAvatarErrored(true)}
            className="h-6 w-6 rounded-md object-cover ring-1 ring-neutral-200"
          />
        ) : (
          <div className="flex h-6 w-6 items-center justify-center rounded-md bg-neutral-100 text-[10px] font-semibold text-neutral-700 ring-1 ring-neutral-200">
            {displayName.charAt(0) || 'U'}
          </div>
        )}
      </button>

      {open && (
        <div className="absolute right-0 top-full z-50 mt-1.5 w-56 overflow-hidden rounded-lg border border-neutral-100 bg-surface-raised shadow-lg">
          <div className="px-4 py-3 border-b border-neutral-100">
            <p className="text-sm font-medium text-neutral-800 truncate">{displayName}</p>
            <p className="truncate text-[11px] text-neutral-500">{user.loginName}</p>
          </div>

          {/* {isAdmin && (
            <div className="py-1">
              <Link
                href="/admin/batch-eval"
                onClick={() => setOpen(false)}
                className="flex items-center gap-2.5 px-4 py-2 text-[13px] text-neutral-700 hover:bg-neutral-50 transition-colors"
              >
                <ImagePlay className="w-4 h-4 text-neutral-400" />
                <span className="flex-1">截图测评</span>
              </Link>
              <Link
                href="/admin/permission"
                onClick={() => setOpen(false)}
                className="flex items-center gap-2.5 px-4 py-2 text-[13px] text-neutral-700 hover:bg-neutral-50 transition-colors"
              >
                <ShieldCheck className="w-4 h-4 text-neutral-400" />
                <span className="flex-1">权限管理</span>
              </Link>
            </div>
          )} */}
        </div>
      )}
    </div>
  );
}

interface AdminTopNavProps {
  onLogoHoverStart?: () => void;
  onLogoHoverEnd?: () => void;
  mobileNavigationOpen?: boolean;
  onMobileNavigationToggle?: () => void;
}

export function AdminTopNav({
  onLogoHoverStart,
  onLogoHoverEnd,
  mobileNavigationOpen = false,
  onMobileNavigationToggle,
}: AdminTopNavProps = {}) {
  const { tenants, currentTenantId } = useAdminConfig();
  const { t } = useLocale();
  const currentTenant = tenants.find((tenant) => tenant.tenantId === currentTenantId);

  return (
    <header className="sticky top-0 z-40 flex h-12 shrink-0 items-center border-b border-neutral-100 bg-surface/90 px-3 backdrop-blur-md sm:px-5">
      {onMobileNavigationToggle && (
        <button
          type="button"
          onClick={onMobileNavigationToggle}
          aria-expanded={mobileNavigationOpen}
          aria-label={mobileNavigationOpen ? t('nav.closeNavigation') : t('nav.openNavigation')}
          className="mr-1 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-neutral-600 transition-colors hover:bg-neutral-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400 md:hidden"
        >
          {mobileNavigationOpen
            ? <X aria-hidden="true" className="h-4 w-4" />
            : <Menu aria-hidden="true" className="h-4 w-4" />}
        </button>
      )}
      <Link
        href="/admin/home"
        aria-label={t('nav.backToWorkbench')}
        className="flex shrink-0 items-center rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
        onMouseEnter={onLogoHoverStart}
        onMouseLeave={onLogoHoverEnd}
      >
        <Image
          src="/agenui-logo.png"
          alt=""
          width={30}
          height={31}
          priority
          className="h-[30px] w-[30px] rounded-lg object-cover shadow-sm ring-1 ring-black/5"
        />
        <span className="ml-2 hidden text-[15px] font-extrabold tracking-tight text-neutral-900 sm:inline" translate="no">AGenUI</span>
      </Link>

      {currentTenant && (
        <div className="ml-3 hidden min-w-0 items-center border-l border-neutral-200 pl-3 sm:flex">
          <span className="max-w-[180px] truncate text-[11px] font-medium text-neutral-600">{currentTenant.tenantName}</span>
        </div>
      )}

      <div className="flex-1" />

      <div className="flex h-9 shrink-0 items-center gap-1 rounded-xl border border-neutral-200/80 bg-surface-raised px-1 shadow-sm">
        <LocaleToggle className="h-7 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400" />
        <ThemeToggle className="h-7 w-7 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400" />
        <UserMenu />
      </div>
    </header>
  );
}
