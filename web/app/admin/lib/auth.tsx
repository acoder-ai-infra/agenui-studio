'use client';

// Open-source build: no SSO. A local identity is provided instead.

import { createContext, useContext, useMemo, type ReactNode } from 'react';
import { useLocale } from '@/lib/i18n/LocaleContext';

export interface AuthUser {
  name: string;
  empId: string;
  role: string;
  DisplayName: string;
  lastName: string;
  loginName: string;
  nickNameCn: string;
  PicURL: string;
  avatarURL: string;
}

interface AuthValue {
  user: AuthUser | null;
  loading: boolean;
  logout: () => void;
}

// This default only applies outside AuthProvider, where no locale is available,
// so it stays on the untranslated identifiers.
const AuthContext = createContext<AuthValue>({
  user: {
    name: 'local',
    empId: 'local',
    role: 'system_super_admin',
    DisplayName: 'local',
    lastName: 'local',
    loginName: 'local',
    nickNameCn: 'local',
    PicURL: '',
    avatarURL: '',
  },
  loading: false,
  logout: () => {},
});

export function AuthProvider({ children }: { children: ReactNode }) {
  const { t } = useLocale();
  const value = useMemo<AuthValue>(
    () => ({
      user: {
        name: 'local',
        empId: 'local',
        role: 'system_super_admin',
        DisplayName: t('user.localName'),
        lastName: 'local',
        loginName: 'local',
        nickNameCn: t('user.localName'),
        PicURL: '',
        avatarURL: '',
      },
      loading: false,
      logout: () => {},
    }),
    [t],
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  return useContext(AuthContext);
}

// Local mode has no SSO redirect. Keeping this no-op makes the auth boundary
// explicit for deployments that provide their own identity adapter.
export function redirectToLogin() {}
