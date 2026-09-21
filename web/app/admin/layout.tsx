'use client';

import { AdminConfigProvider } from './context';
import { AuthProvider } from './lib/auth';
import { AdminLayoutInner } from './components/AdminLayoutInner';

export default function AdminLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <AuthProvider>
      <AdminConfigProvider>
        <AdminLayoutInner>{children}</AdminLayoutInner>
      </AdminConfigProvider>
    </AuthProvider>
  );
}
