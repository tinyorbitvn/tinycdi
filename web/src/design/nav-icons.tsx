import type { ReactNode } from "react";
import { cx } from "./cx";
import type { IconProps } from "./icons";

// Orbit 2.9 "Dashboard icons" — duotone artwork (24 grid, 1.75 stroke,
// currentColor + a 0.16 fill layer), custom TinyOrbit design-system art
// used with permission of the brand owner (see TRADEMARKS.md / the orbit
// README). Sidebar nav only — everywhere else uses the Tabler outline set
// in ./icons.tsx. Same aria contract as makeIcon: decorative by default
// (aria-hidden), pass `title` to make one meaningful. Source SVGs drop
// their baked-in role/aria-label/title — the wrapper owns them.

function makeNavIcon(name: string, body: ReactNode) {
  function Icon({ size = 20, title, className, ...rest }: IconProps) {
    return (
      <svg
        xmlns="http://www.w3.org/2000/svg"
        viewBox="0 0 24 24"
        width={size}
        height={size}
        fill="none"
        stroke="currentColor"
        strokeWidth={1.75}
        strokeLinecap="round"
        strokeLinejoin="round"
        focusable="false"
        className={cx("tc-icon", "tc-nav-icon", className)}
        {...(title ? { role: "img", "aria-label": title } : { "aria-hidden": true })}
        {...rest}
      >
        {title ? <title>{title}</title> : null}
        {body}
      </svg>
    );
  }
  Icon.displayName = `NavIcon${name}`;
  return Icon;
}

// "vps" — stacked servers; the Workspaces section.
export const NavIconVps = makeNavIcon(
  "Vps",
  <>
    <rect x="3" y="4" width="18" height="7" rx="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <rect x="3" y="14" width="18" height="7" rx="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <rect x="3" y="4" width="18" height="7" rx="2" />
    <rect x="3" y="14" width="18" height="7" rx="2" />
    <path d="M7 7.5h.1m-.1 10h.1M12 7.5h5m-5 10h5" />
  </>,
);

// "services" — app tiles + add; the Template catalog section.
export const NavIconServices = makeNavIcon(
  "Services",
  <>
    <rect x="3" y="3" width="7" height="7" rx="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <rect x="14" y="3" width="7" height="7" rx="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <rect x="3" y="14" width="7" height="7" rx="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <rect x="3" y="3" width="7" height="7" rx="2" />
    <rect x="14" y="3" width="7" height="7" rx="2" />
    <rect x="3" y="14" width="7" height="7" rx="2" />
    <path d="M14 17.5h7m-3.5-3.5v7" />
  </>,
);

// "cloud" — retained disks persisted outside a workspace; the Retained
// data section.
export const NavIconCloud = makeNavIcon(
  "Cloud",
  <>
    <path
      d="M7 19h11a5 5 0 0 0 1-10A7 7 0 0 0 5 9a5 5 0 0 0 2 10Z"
      fill="currentColor"
      fillOpacity={0.16}
      stroke="none"
    />
    <path d="M7 19h11a5 5 0 0 0 1-10A7 7 0 0 0 5 9a5 5 0 0 0 2 10Z" />
  </>,
);

// "settings" — sliders; the Admin section.
export const NavIconSettings = makeNavIcon(
  "Settings",
  <>
    <circle cx="8" cy="6" r="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <circle cx="16" cy="12" r="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <circle cx="10" cy="18" r="2" fill="currentColor" fillOpacity={0.16} stroke="none" />
    <path d="M3 6h3m4 0h11M3 12h11m4 0h3M3 18h5m4 0h9" />
    <circle cx="8" cy="6" r="2" />
    <circle cx="16" cy="12" r="2" />
    <circle cx="10" cy="18" r="2" />
  </>,
);
