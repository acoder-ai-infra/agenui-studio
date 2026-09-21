'use client';

import React, { useState } from 'react';
import { LogIn } from 'lucide-react';
import { useAuth, redirectToLogin } from '../lib/auth';
import { useAdminConfig } from '../context';

export function AdminTopBar() {
  const { user, loading } = useAuth();
  const { tenants, currentTenantId } = useAdminConfig();
  const [avatarErrored, setAvatarErrored] = useState(false);

  const displayName = user?.DisplayName || user?.nickNameCn || user?.lastName || user?.loginName || '';
  const avatarUrl = user?.PicURL || user?.avatarURL || '';
  const showAvatarImg = !!avatarUrl && !avatarErrored;

  // 当前空间名称
  const currentTenant = tenants.find((t) => t.tenantId === currentTenantId);
  const spaceName = currentTenant?.tenantName ?? '';
  const visibilityLabel = currentTenant?.isSystem ? '系统空间' : currentTenant?.isPublic ? '公共空间' : '私有空间';

  return (
    <header className="h-12 shrink-0 flex items-center justify-between px-5 bg-surface border-b border-neutral-100">
      {/* 左侧：当前空间标识 */}
      <div className="flex items-center gap-2">
        {spaceName && (
          <span className="text-sm text-neutral-600">
            当前：{spaceName}{' '}
            <span className="text-xs text-neutral-400">[{visibilityLabel}]</span>
          </span>
        )}
      </div>

      {/* 右侧：用户信息 */}
      <div className="flex items-center gap-3">
        {loading ? (
          <div aria-hidden="true" className="h-5 w-24 animate-pulse rounded bg-neutral-100 motion-reduce:animate-none" />
        ) : user ? (
          <div className="flex items-center gap-2">
            {showAvatarImg ? (
              // eslint-disable-next-line @next/next/no-img-element
              <img
                src={avatarUrl}
                alt={displayName}
                width={28}
                height={28}
                onError={() => setAvatarErrored(true)}
                className="w-7 h-7 rounded-full object-cover ring-1 ring-neutral-200"
              />
            ) : (
              <div className="w-7 h-7 rounded-full bg-primary-100 flex items-center justify-center text-xs font-medium text-primary-700">
                {displayName.charAt(0) || 'U'}
              </div>
            )}
            <span className="text-sm text-neutral-700">{displayName}</span>
          </div>
        ) : (
          <button
            type="button"
            onClick={() => redirectToLogin()}
            className="inline-flex items-center gap-1.5 rounded-lg bg-inverse px-3 py-1.5 text-xs font-medium text-inverse-fg transition-colors hover:bg-inverse-hover"
          >
            <LogIn aria-hidden="true" className="h-3.5 w-3.5" />
            登录
          </button>
        )}
      </div>
    </header>
  );
}
