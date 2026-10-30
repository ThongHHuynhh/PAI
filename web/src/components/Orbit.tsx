import { useEffect, useRef, useState } from "react";

// Orbit is PAI's mark, as the TUI draws it: a lit core with machines on
// tilted orbits around it and a pulse running along each link. still draws
// one frame, for small places such as the nav.
const RINGS = [
  { r: 0.55, inc: 12, node: 0, speed: 0.07 },
  { r: 0.78, inc: -24, node: 50, speed: -0.05 },
  { r: 1.0, inc: 38, node: -35, speed: 0.035 },
];
const RAD = Math.PI / 180;
const PITCH = 70; // degrees: rings seen from a little above

function point(i: number, th: number): [number, number, number] {
  const g = RINGS[i];
  let x = g.r * Math.cos(th), y = 0, z = g.r * Math.sin(th);
  const ci = Math.cos(g.inc * RAD), si = Math.sin(g.inc * RAD);
  [y, z] = [y * ci - z * si, y * si + z * ci];
  const cn = Math.cos(g.node * RAD), sn = Math.sin(g.node * RAD);
  [x, z] = [x * cn + z * sn, -x * sn + z * cn];
  const cp = Math.cos(PITCH * RAD), sp = Math.sin(PITCH * RAD);
  [y, z] = [y * cp - z * sp, y * sp + z * cp];
  return [x, y, z];
}

export function Orbit({ size, nodes = 6, still = false }: { size: number; nodes?: number; still?: boolean }) {
  const [t, setT] = useState(4);
  const raf = useRef(0);
  useEffect(() => {
    if (still || window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) return;
    const start = performance.now();
    const tick = (now: number) => {
      setT(4 + (now - start) / 1000);
      raf.current = requestAnimationFrame(tick);
    };
    raf.current = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf.current);
  }, [still]);
  const R = 46;
  const core = 9 * (1 + 0.06 * Math.sin(t * 0.6 * 2 * Math.PI));
  const ringPaths = RINGS.map((_, i) => {
    const pts = Array.from({ length: 73 }, (_, k) => point(i, (k / 72) * 2 * Math.PI));
    return pts.map(([x, y], k) => `${k ? "L" : "M"}${(50 + x * R).toFixed(2)},${(50 - y * R).toFixed(2)}`).join("");
  });
  const ns = Array.from({ length: Math.max(1, Math.min(12, nodes)) }, (_, j) => {
    const i = j % RINGS.length;
    const th = 2 * Math.PI * (RINGS[i].speed * t + j / nodes) + i;
    const [x, y, z] = point(i, th);
    const f = (t * 0.5 + j * 0.37) % 1;
    const p = j % 2 ? 1 - f : f;
    return { x: 50 + x * R, y: 50 - y * R, z, p, j };
  });
  return (
    <svg width={size} height={size} viewBox="0 0 100 100" className="orbit" aria-hidden>
      <defs>
        <radialGradient id="pai-core" cx="38%" cy="35%" r="70%">
          <stop offset="0%" stopColor="var(--accent-hi)" />
          <stop offset="60%" stopColor="var(--accent)" />
          <stop offset="100%" stopColor="var(--accent-lo)" />
        </radialGradient>
      </defs>
      {ringPaths.map((d, i) => (
        <path key={i} d={d} fill="none" stroke="var(--ring)" strokeWidth={0.9} strokeDasharray="1 1.6" />
      ))}
      {!still && (
        <circle cx={50} cy={50} r={core * (1 + 2.2 * ((t * 0.45) % 1))} fill="none" stroke="var(--accent)" strokeOpacity={1 - ((t * 0.45) % 1)} strokeWidth={0.7} />
      )}
      {ns
        .filter((n) => n.z < 0)
        .map((n) => (
          <circle key={n.j} cx={n.x} cy={n.y} r={1.6} fill="var(--node)" opacity={0.55} />
        ))}
      <circle cx={50} cy={50} r={core} fill="url(#pai-core)" />
      {ns
        .filter((n) => n.z >= 0)
        .map((n) => (
          <g key={n.j}>
            <line x1={50} y1={50} x2={n.x} y2={n.y} stroke="var(--ring)" strokeWidth={0.6} strokeDasharray="1.2 1.3" />
            {!still && <circle cx={50 + (n.x - 50) * n.p} cy={50 + (n.y - 50) * n.p} r={1.3} fill="var(--accent)" />}
            <circle cx={n.x} cy={n.y} r={2.2 + n.z} fill="var(--node)" />
          </g>
        ))}
    </svg>
  );
}
