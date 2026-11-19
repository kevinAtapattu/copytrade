import { logo } from "@/lib/logo";

// Browser tab icon.
export const size = { width: 64, height: 64 };
export const contentType = "image/png";
export default function Icon() {
  return logo(64);
}
