import type { Metadata, Viewport } from "next";
import "./globals.css";
import Shell from "@/components/Shell";

export const metadata: Metadata = {
  title: "copytrade",
  description: "pump.fun copyability research",
  // Opened from the iPhone home screen: full screen, named "copytrade".
  appleWebApp: { capable: true, title: "copytrade", statusBarStyle: "black" },
};

export const viewport: Viewport = { themeColor: "#000000" };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Shell>{children}</Shell>
      </body>
    </html>
  );
}
