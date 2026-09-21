import type { Config } from "tailwindcss";

/**
 * 颜色必须写成 `rgb(var(--x) / <alpha-value>)` 而不是裸 `var(--x)`。
 * Tailwind 3 的 asColor() 解析不了裸 var()，会让 `bg-neutral-50/60`
 * 这类带透明度修饰的类整个从产物里消失（渲染成透明）。
 * 对应地，globals.css 里的变量值是空格分隔的 RGB 通道，不带 rgb()。
 */
const ramp = (name: string) => `rgb(var(--c-${name}) / <alpha-value>)`;

const STOPS = [50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 950] as const;

const scale = (name: string) =>
  Object.fromEntries(STOPS.map((s) => [s, ramp(`${name}-${s}`)]));

const config: Config = {
  content: [
    "./pages/**/*.{js,ts,jsx,tsx,mdx}",
    "./components/**/*.{js,ts,jsx,tsx,mdx}",
    "./app/**/*.{js,ts,jsx,tsx,mdx}",
    "./lib/**/*.{js,ts,jsx,tsx,mdx}",
  ],
  // 只认 .dark 类。不要加回 'media'：营销站有 236 处手写 dark: 变体，
  // 加了 'media' 它们会跟着系统偷偷生效，而营销站本轮应保持亮色不变。
  // 系统跟随由 lib/theme/ThemeContext.tsx 负责（仅 /admin 生效）。
  darkMode: 'class',
  theme: {
    screens: {
      'sm': '640px',
      'md': '768px',
      'lg': '1024px',
      'xl': '1280px',
      '2xl': '1536px',
      '3xl': '2000px',
    },
    extend: {
      colors: {
        background: "var(--background)",
        foreground: "var(--foreground)",

        // 语义面 —— 换肤时只改 globals.css 里的变量，不用碰组件
        surface: ramp('surface'),
        'surface-raised': ramp('surface-raised'),
        'surface-sunken': ramp('surface-sunken'),
        line: ramp('line'),
        'line-strong': ramp('line-strong'),
        inverse: {
          DEFAULT: ramp('inverse'),
          hover: ramp('inverse-hover'),
          fg: ramp('inverse-fg'),
        },
        info: {
          DEFAULT: ramp('info'),
          hover: ramp('info-hover'),
          fg: ramp('info-fg'),
        },

        // 聊天用户气泡。亮色下等于 inverse，暗色下是低对比的浅色填充——
        // 反色面在深底上会变成白底黑字，一整块抢掉活动流的注意力。
        bubble: {
          DEFAULT: ramp('bubble'),
          fg: ramp('bubble-fg'),
        },
        // 「进行中」的进度色：亮色 blue-500，暗色青绿（深底上蓝色掉到读不了）
        progress: ramp('progress'),

        // 变量化的调色 ramp。亮/暗值都在 app/globals.css 的 :root / .dark 里。
        neutral: scale('neutral'),
        violet: scale('violet'),
        red: scale('red'),
        amber: scale('amber'),
        emerald: scale('emerald'),
        sky: scale('sky'),
        rose: scale('rose'),

        // 代码块两种皮肤下都是深色。底色仍走变量：暗色下要比面板更深，
        // 否则 #171717 贴在 #171B23 的面板上几乎看不出边界。前景固定。
        code: {
          DEFAULT: ramp('code'),
          fg: '#f5f5f5',
          dim: '#d4d4d4',
        },

        // 设备外壳 / AGenUI 卡片容器。固定 hex，不参与 .dark 反转：
        // 这里的明暗由被预览卡片自己的 colorScheme 决定，与 Studio 皮肤解耦。
        // 用 ramp 会得到"亮色边框包着暗色画布"。
        device: {
          canvas: '#ffffff',
          'canvas-dark': '#1a1a1a',
          bezel: '#262626',
          'bezel-dark': '#525252',
          edge: '#e5e5e5',
          'edge-soft': '#f5f5f5',
          'edge-dark': '#404040',
          ink: '#262626',
          'ink-dark': '#e5e5e5',
          icon: '#404040',
          'icon-dark': '#d4d4d4',
          bar: '#262626',
          'bar-dark': '#737373',
        },

        primary: {
          50: '#edfffb',
          100: '#cffff2',
          200: '#9fffe5',
          300: '#62f6d2',
          400: '#28e6bc',
          500: '#00cfa5',
          600: '#00a786',
          700: '#00806a',
          800: '#005f50',
          900: '#00463b',
        },
        accent: {
          purple: '#008cff',
          pink: '#ff2d92',
          cyan: '#00c7ff',
        }
      },
      fontFamily: {
        sans: ["var(--font-geist-sans)", 'Inter', 'system-ui', '-apple-system', 'sans-serif'],
        mono: ["var(--font-geist-mono)", 'Menlo', 'Monaco', 'Courier New', 'monospace'],
      },
      fontSize: {
        'xs': ['0.75rem', { lineHeight: '1rem' }],
        'sm': ['0.875rem', { lineHeight: '1.25rem' }],
        'base': ['1rem', { lineHeight: '1.5rem' }],
        'lg': ['1.125rem', { lineHeight: '1.75rem' }],
        'xl': ['1.25rem', { lineHeight: '1.75rem' }],
        '2xl': ['1.5rem', { lineHeight: '2rem' }],
        '3xl': ['1.875rem', { lineHeight: '2.25rem' }],
        '4xl': ['2.25rem', { lineHeight: '2.5rem' }],
        '5xl': ['3rem', { lineHeight: '1.16' }],
        '6xl': ['3.75rem', { lineHeight: '1.16' }],
        '7xl': ['4.5rem', { lineHeight: '1.16' }],
        '8xl': ['6rem', { lineHeight: '1.16' }],
        '9xl': ['8rem', { lineHeight: '1.16' }],
      },
      spacing: {
        '18': '4.5rem',
        '88': '22rem',
        '100': '25rem',
        '112': '28rem',
        '128': '32rem',
      },
      borderRadius: {
        '4xl': '2rem',
        lg: "var(--radius)",
        md: "calc(var(--radius) - 2px)",
        sm: "calc(var(--radius) - 4px)",
      },
      boxShadow: {
        'soft': '0 2px 8px 0 rgba(0, 0, 0, 0.05)',
        'medium': '0 4px 16px 0 rgba(0, 0, 0, 0.08)',
        'large': '0 8px 32px 0 rgba(0, 0, 0, 0.12)',
        'glow': '0 0 24px rgba(0, 199, 255, 0.28)',
        'glow-lg': '0 0 48px rgba(255, 45, 146, 0.28)',
      },
      animation: {
        'fade-in': 'fadeIn 0.6s ease-out',
        'fade-in-up': 'fadeInUp 0.6s ease-out',
        'fade-in-down': 'fadeInDown 0.6s ease-out',
        'slide-in-left': 'slideInLeft 0.6s ease-out',
        'slide-in-right': 'slideInRight 0.6s ease-out',
        'scale-in': 'scaleIn 0.4s ease-out',
        'float': 'float 3s ease-in-out infinite',
        'pulse-slow': 'pulse 3s cubic-bezier(0.4, 0, 0.6, 1) infinite',
        'gradient': 'gradient 8s linear infinite',
        'composer-feed-in': 'composerFeedIn 0.42s cubic-bezier(0.2, 0.75, 0.25, 1) both',
        'composer-activity-spin': 'composerActivitySpin 0.9s linear infinite',
        'composer-detail-in': 'composerDetailIn 0.2s ease both',
        'composer-dot-pulse': 'composerDotPulse 1.15s ease-in-out infinite',
      },
      keyframes: {
        fadeIn: {
          '0%': { opacity: '0' },
          '100%': { opacity: '1' },
        },
        fadeInUp: {
          '0%': { opacity: '0', transform: 'translateY(20px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        fadeInDown: {
          '0%': { opacity: '0', transform: 'translateY(-20px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        slideInLeft: {
          '0%': { opacity: '0', transform: 'translateX(-20px)' },
          '100%': { opacity: '1', transform: 'translateX(0)' },
        },
        slideInRight: {
          '0%': { opacity: '0', transform: 'translateX(20px)' },
          '100%': { opacity: '1', transform: 'translateX(0)' },
        },
        scaleIn: {
          '0%': { opacity: '0', transform: 'scale(0.95)' },
          '100%': { opacity: '1', transform: 'scale(1)' },
        },
        float: {
          '0%, 100%': { transform: 'translateY(0)' },
          '50%': { transform: 'translateY(-10px)' },
        },
        gradient: {
          '0%, 100%': { backgroundPosition: '0% 50%' },
          '50%': { backgroundPosition: '100% 50%' },
        },
        composerFeedIn: {
          '0%': { opacity: '0', transform: 'translateY(9px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        composerActivitySpin: {
          '100%': { transform: 'rotate(360deg)' },
        },
        composerDetailIn: {
          '0%': { opacity: '0', transform: 'translateY(-3px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        composerDotPulse: {
          '0%, 60%, 100%': { opacity: '0.28', transform: 'translateY(0)' },
          '30%': { opacity: '1', transform: 'translateY(-2px)' },
        },
      },
      backgroundImage: {
        'gradient-radial': 'radial-gradient(var(--tw-gradient-stops))',
        'gradient-conic': 'conic-gradient(from 180deg at 50% 50%, var(--tw-gradient-stops))',
        'gradient-mesh': 'linear-gradient(120deg, #32d74b 0%, #00e5a8 22%, #00c7ff 50%, #008cff 76%, #ff2d92 100%)',
      },
      backdropBlur: {
        xs: '2px',
      },
    },
  },
  plugins: [],
};

export default config;
