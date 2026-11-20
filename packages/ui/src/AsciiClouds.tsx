import { useEffect, useRef } from "react";
import { initAsciiClouds } from "./asciiCloudRenderer";

export function AsciiClouds() {
  const host = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const layer = host.current;
    if (!layer) return;
    const preference = matchMedia("(prefers-reduced-motion: reduce)");
    let dispose = () => {};
    const start = () => {
      dispose();
      const canvas = document.createElement("canvas");
      layer.append(canvas);
      let stop: (() => void) | undefined;
      const release = () => {
        stop?.();
        canvas
          .getContext("webgl2")
          ?.getExtension("WEBGL_lose_context")
          ?.loseContext();
        canvas.remove();
      };
      dispose = release;
      try {
        stop = initAsciiClouds(canvas);
      } catch {
        /* Keep the neutral background. */
      }
      if (!stop) release();
    };
    start();
    preference.addEventListener("change", start);
    return () => {
      preference.removeEventListener("change", start);
      dispose();
    };
  }, []);
  return <div className="ascii-clouds" ref={host} aria-hidden="true" />;
}
