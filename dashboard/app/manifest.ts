import type { MetadataRoute } from "next";

// Lets "Add to Home Screen" open the dashboard full screen, like an app.
export default function manifest(): MetadataRoute.Manifest {
  return {
    name: "copytrade",
    short_name: "copytrade",
    start_url: "/",
    display: "standalone",
    background_color: "#000000",
    theme_color: "#000000",
    icons: [
      { src: "/apple-icon", sizes: "180x180", type: "image/png" },
      { src: "/logo512", sizes: "512x512", type: "image/png" },
    ],
  };
}
