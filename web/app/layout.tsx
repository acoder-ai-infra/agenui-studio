import type { Metadata, Viewport } from "next";
import "./globals.css";
import { LocaleProvider } from "@/lib/i18n/LocaleContext";
import { ThemeProvider } from "@/lib/theme/ThemeContext";
import { THEME_BOOTSTRAP_SCRIPT, THEME_COLORS } from "@/lib/theme/config";

export const metadata: Metadata = {
  title: "AGenUI Studio",
  description:
    "A self-hosted workspace for building and iterating on agent-generated interfaces.",
};

export const viewport: Viewport = {
  themeColor: THEME_COLORS.light,
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    // `lang` is the server-rendered default and matches LocaleProvider's server
    // snapshot; LocaleProvider takes over the attribute once mounted.
    <html lang="zh-CN" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_BOOTSTRAP_SCRIPT }} />
      </head>
      <body className="antialiased">
        <LocaleProvider>
          <ThemeProvider>{children}</ThemeProvider>
        </LocaleProvider>
      </body>
    </html>
  );
}
