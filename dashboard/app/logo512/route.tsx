import { logo } from "@/lib/logo";

// Large icon for the web app manifest (Android and desktop installs).
export function GET() {
  return logo(512);
}
