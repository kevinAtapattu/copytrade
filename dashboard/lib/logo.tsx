import { ImageResponse } from "next/og";

// The app mark: a white "c." on black. The c is an open ring (gap on the right)
// with round ends; the dot sits on its baseline. Drawn on a 100x100 grid and
// centered; iOS rounds the corners of home-screen icons itself.
export function logo(size: number) {
  return new ImageResponse(
    (
      <svg width={size} height={size} viewBox="0 0 100 100" xmlns="http://www.w3.org/2000/svg">
        <rect width="100" height="100" fill="#000" />
        <path d="M 59.56 34.44 A 22 22 0 1 0 59.56 65.56" fill="none" stroke="#fff" strokeWidth="13" strokeLinecap="round" />
        <circle cx="78" cy="71.5" r="7" fill="#fff" />
      </svg>
    ),
    { width: size, height: size },
  );
}
