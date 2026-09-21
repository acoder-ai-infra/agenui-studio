'use client';

import {
  createContext,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from 'react';
import { useLocale } from '@/lib/i18n/LocaleContext';

export type TenantRole = 'tenant_member' | 'tenant_admin' | 'system_super_admin';

export interface TenantInfo {
  id: number;
  tenantId: number;
  name: string;
  tenantName: string;
  isPublic?: boolean;
  isSystem?: boolean;
}

interface AdminConfigValue {
  tenants: TenantInfo[];
  currentTenantId: number;
  setCurrentTenantId: (id: number) => void;
  tenantsLoading: boolean;
  isAdmin: boolean;
  isSystemTenant: boolean;
  tenantRole: TenantRole;
  model: string;
  setModel: (m: string) => void;
  styleId: string;
  setStyleId: (s: string) => void;
  protocolVersion: string;
  setProtocolVersion: (v: string) => void;
}

const AdminConfigContext = createContext<AdminConfigValue | null>(null);

// Open-source build: a single local workspace instead of multi-tenancy.
export function AdminConfigProvider({ children }: { children: ReactNode }) {
  const { t } = useLocale();
  const [currentTenantId, setCurrentTenantId] = useState(1);
  const [model, setModel] = useState('mock');
  const [styleId, setStyleId] = useState('agenui-studio-default');
  const [protocolVersion, setProtocolVersion] = useState('v0.9');

  const value = useMemo<AdminConfigValue>(
    () => ({
      tenants: [
        {
          id: 1,
          tenantId: 1,
          name: t('space.localWorkspace'),
          tenantName: t('space.localWorkspace'),
          isPublic: false,
          isSystem: true,
        },
      ],
      currentTenantId,
      setCurrentTenantId,
      tenantsLoading: false,
      isAdmin: true,
      isSystemTenant: true,
      tenantRole: 'system_super_admin',
      model,
      setModel,
      styleId,
      setStyleId,
      protocolVersion,
      setProtocolVersion,
    }),
    [currentTenantId, model, styleId, protocolVersion, t],
  );

  return (
    <AdminConfigContext.Provider value={value}>
      {children}
    </AdminConfigContext.Provider>
  );
}

export function useAdminConfig() {
  const ctx = useContext(AdminConfigContext);
  if (!ctx) {
    throw new Error('useAdminConfig must be used within AdminConfigProvider');
  }
  return ctx;
}
