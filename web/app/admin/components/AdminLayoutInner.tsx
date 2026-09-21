'use client';

import { useEffect, useState } from 'react';
import { usePathname } from 'next/navigation';
import { useLocale } from '@/lib/i18n/LocaleContext';
import { AdminSidebar } from './AdminSidebar';
import { AdminTopNav } from './AdminTopNav';

export function AdminLayoutInner({ children }: { children: React.ReactNode }) {
  const [mobileNavigationOpen, setMobileNavigationOpen] = useState(false);
  const pathname = usePathname();
  const { t } = useLocale();

  useEffect(() => {
    setMobileNavigationOpen(false);
  }, [pathname]);

  return (
    <div className="admin-layout flex flex-col h-screen overflow-hidden bg-surface relative z-10 isolate">
      <AdminTopNav
        mobileNavigationOpen={mobileNavigationOpen}
        onMobileNavigationToggle={() => setMobileNavigationOpen((open) => !open)}
      />
      <div className="flex flex-1 overflow-hidden">
        <div className="hidden md:flex">
          <AdminSidebar />
        </div>
        {mobileNavigationOpen && (
          <>
            <button
              type="button"
              aria-label={t('nav.closeNavigation')}
              onClick={() => setMobileNavigationOpen(false)}
              className="fixed inset-x-0 bottom-0 top-12 z-20 bg-neutral-950/25 backdrop-blur-[1px] md:hidden"
            />
            <div className="fixed bottom-0 left-0 top-12 z-30 flex shadow-2xl md:hidden">
              <AdminSidebar />
            </div>
          </>
        )}
        <main className="min-w-0 flex-1 overflow-auto">
          {children}
        </main>
      </div>
    </div>
  );
}
