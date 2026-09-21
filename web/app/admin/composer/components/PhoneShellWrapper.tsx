'use client';

import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { ColorScheme } from '@/lib/agenui-types';
import { cn } from '@/lib/utils';

const PHONE_WIDTH = 393;
const PHONE_HEIGHT = 852;

export type PhoneShellPlatform = 'ios' | 'android';

interface PhoneShellWrapperProps {
  children: ReactNode;
  colorScheme?: ColorScheme;
  className?: string;
  deviceWidth?: number;
  deviceHeight?: number;
  platform?: PhoneShellPlatform;
}

export function PhoneShellWrapper({
  children,
  colorScheme = 'light',
  className,
  deviceWidth = PHONE_WIDTH,
  deviceHeight = PHONE_HEIGHT,
  platform = 'ios',
}: PhoneShellWrapperProps) {
  const isDark = colorScheme === 'dark';
  const isIOS = platform === 'ios';
  const logicalWidth = deviceWidth;
  const logicalHeight = deviceHeight;
  const statusBarRatio = (isIOS ? 54 : 44) / logicalHeight;
  const homeBarRatio = (isIOS ? 30 : 28) / logicalHeight;
  const outerRadius = isIOS ? 54 : 42;
  const innerRadius = isIOS ? 47 : 36;
  const hostRef = useRef<HTMLDivElement>(null);
  const [scale, setScale] = useState(1);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    const updateScale = () => {
      const rect = host.getBoundingClientRect();
      // Keep the DSL at the selected device's logical viewport and scale the
      // complete shell as one unit so responsive behavior matches Runtime.
      const availableWidth = Math.max(1, rect.width - 40);
      const availableHeight = Math.max(1, rect.height - 24);
      setScale(Math.min(1, availableWidth / logicalWidth, availableHeight / logicalHeight));
    };
    updateScale();
    const observer = new ResizeObserver(updateScale);
    observer.observe(host);
    return () => observer.disconnect();
  }, [logicalHeight, logicalWidth]);

  return (
    <div ref={hostRef} className={cn('flex h-full w-full flex-col items-center justify-center px-5 py-3', className)}>
      <div style={{ width: logicalWidth * scale, height: logicalHeight * scale }}>
        {/* Keep the DSL at the Runtime coordinate size; this shell is scaled as
            one unit for preview only. */}
        <div
          style={{
            width: logicalWidth,
            height: logicalHeight,
            transform: `scale(${scale})`,
            transformOrigin: 'top left',
          }}
        >
          <div
            data-preview-device-frame
            data-device-width={logicalWidth}
            data-device-height={logicalHeight}
            data-device-platform={platform}
            className={cn(
              'relative box-border flex h-full w-full bg-[#171A1E] shadow-[0_18px_40px_rgba(24,35,43,0.18),0_3px_10px_rgba(24,35,43,0.12)] ring-1 ring-black/20',
              isIOS ? 'p-[7px]' : 'p-[6px]',
            )}
            style={{ borderRadius: outerRadius }}
          >
        {/* 侧边实体按键 */}
        {isIOS ? (
          <>
            <span aria-hidden="true" className="absolute -left-[4px] top-[15%] h-[3.5%] w-[4px] rounded-l-sm bg-[#343940]" />
            <span aria-hidden="true" className="absolute -left-[4px] top-[22%] h-[7%] w-[4px] rounded-l-sm bg-[#343940]" />
            <span aria-hidden="true" className="absolute -left-[4px] top-[31%] h-[7%] w-[4px] rounded-l-sm bg-[#343940]" />
            <span aria-hidden="true" className="absolute -right-[4px] top-[24%] h-[10%] w-[4px] rounded-r-sm bg-[#343940]" />
          </>
        ) : (
          <>
            <span aria-hidden="true" className="absolute -right-[4px] top-[18%] h-[6%] w-[4px] rounded-r-sm bg-[#343940]" />
            <span aria-hidden="true" className="absolute -right-[4px] top-[27%] h-[11%] w-[4px] rounded-r-sm bg-[#343940]" />
          </>
        )}

        <div
          data-color-scheme={colorScheme}
          className={cn(
            'relative flex min-h-0 w-full flex-1 flex-col overflow-hidden ring-1 ring-inset',
            isDark
              ? 'bg-device-canvas-dark ring-white/10'
              : 'bg-device-canvas ring-black/10',
          )}
          style={{ borderRadius: innerRadius, colorScheme }}
        >
          {/* 状态栏与安全区 */}
          <div
            className="relative flex shrink-0 items-center justify-between px-[7.5%] pt-[1%]"
            style={{ height: `${statusBarRatio * 100}%` }}
          >
            {isIOS ? (
              <div className="absolute left-1/2 top-[48%] z-10 aspect-[4/1] w-[30%] -translate-x-1/2 -translate-y-1/2 rounded-full bg-black shadow-[inset_0_0_0_1px_rgba(255,255,255,0.04)]" />
            ) : (
              <div className="absolute left-1/2 top-[44%] z-10 aspect-square w-[3.8%] -translate-x-1/2 -translate-y-1/2 rounded-full bg-black shadow-[inset_0_0_0_1px_rgba(255,255,255,0.08)]" />
            )}
            <span className={cn('text-[12px] font-semibold tracking-tight', isDark ? 'text-device-ink-dark' : 'text-device-ink')}>
              9:41
            </span>
            <div className="flex items-center gap-1">
              <svg aria-hidden="true" width="15" height="11" viewBox="0 0 16 12" className={cn('opacity-75', isDark ? 'text-device-icon-dark' : 'text-device-icon')}>
                <rect x="0" y="8" width="3" height="4" rx="0.5" fill="currentColor" />
                <rect x="4.5" y="5" width="3" height="7" rx="0.5" fill="currentColor" />
                <rect x="9" y="2" width="3" height="10" rx="0.5" fill="currentColor" />
                <rect x="13" y="0" width="3" height="12" rx="0.5" fill="currentColor" />
              </svg>
              <svg aria-hidden="true" width="13" height="11" viewBox="0 0 14 12" className={cn('opacity-75', isDark ? 'text-device-icon-dark' : 'text-device-icon')}>
                <path d="M7 3.5C8.8 3.5 10.4 4.2 11.6 5.3L13 3.9C11.4 2.4 9.3 1.5 7 1.5S2.6 2.4 1 3.9L2.4 5.3C3.6 4.2 5.2 3.5 7 3.5Z" fill="currentColor" />
                <path d="M7 6.5C8.1 6.5 9.1 6.9 9.8 7.6L11.2 6.2C10.1 5.2 8.6 4.5 7 4.5S3.9 5.2 2.8 6.2L4.2 7.6C4.9 6.9 5.9 6.5 7 6.5Z" fill="currentColor" />
                <circle cx="7" cy="10" r="1.5" fill="currentColor" />
              </svg>
              <div aria-hidden="true" className={cn('flex items-center opacity-75', isDark ? 'text-device-icon-dark' : 'text-device-icon')}>
                <div className="h-[9px] w-[20px] rounded-[3px] border border-current p-[1px]">
                  <div className="h-full w-[78%] rounded-[1px] bg-current" />
                </div>
                <div className="ml-[1px] h-[4px] w-[1.5px] rounded-r-[1px] bg-current" />
              </div>
            </div>
          </div>

          {/* 内容区 */}
          <div className="min-h-0 flex-1 overflow-x-hidden overflow-y-auto overscroll-contain">
            {children}
          </div>

          {/* Home Indicator 安全区 */}
          <div
            className="flex shrink-0 items-center justify-center"
            style={{ height: `${homeBarRatio * 100}%` }}
          >
            <div
              className={cn(
                'h-[5px] rounded-full',
                isIOS ? 'w-[34%]' : 'w-[30%]',
                isDark ? 'bg-device-bar-dark' : 'bg-device-bar',
              )}
            />
          </div>
        </div>
          </div>
        </div>
      </div>
    </div>
  );
}
