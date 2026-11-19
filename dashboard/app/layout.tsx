import type { Metadata, Viewport } from "next";
import "./globals.css";
import Shell from "@/components/Shell";

export const metadata: Metadata = {
  title: "copytrade",
  description: "pump.fun copyability research",
  // Opened from the iPhone home screen: full screen, named "copytrade".
  appleWebApp: { capable: true, title: "copytrade", statusBarStyle: "black" },
};

// cover: the page runs under the iPhone's rounded corners and home bar; the tab bar
// pads itself with the safe-area insets.
export const viewport: Viewport = { themeColor: "#000000", viewportFit: "cover" };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Shell>{children}</Shell>
      </body>
    </html>
  );
}
