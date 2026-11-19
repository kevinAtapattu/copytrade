import { logo } from "@/lib/logo";

// Home-screen icon on iPhone ("Add to Home Screen").
export const size = { width: 180, height: 180 };
export const contentType = "image/png";
export default function AppleIcon() {
  return logo(180);
}
