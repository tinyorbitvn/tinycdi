import type { ReactNode, SVGProps } from "react";
import { cx } from "./cx";

// Small inline SVG icon set (24×24 grid, 1.75px round strokes — Orbit
// normalizes Tabler's 2px — currentColor).
// Decorative by default (aria-hidden); pass `title` to make one meaningful.

export type IconSize = 12 | 14 | 16 | 20 | 24 | 32 | 48;

export interface IconProps extends Omit<SVGProps<SVGSVGElement>, "children" | "style"> {
  size?: IconSize;
  /** Accessible name; omit for decorative icons. */
  title?: string;
}

function makeIcon(name: string, body: ReactNode) {
  function Icon({ size = 16, title, className, ...rest }: IconProps) {
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
        className={cx("tc-icon", className)}
        {...(title ? { role: "img", "aria-label": title } : { "aria-hidden": true })}
        {...rest}
      >
        {title ? <title>{title}</title> : null}
        {body}
      </svg>
    );
  }
  Icon.displayName = `Icon${name}`;
  return Icon;
}

export const IconPlus = makeIcon("Plus", <path d="M12 5v14M5 12h14" />);
export const IconMinus = makeIcon("Minus", <path d="M5 12h14" />);
export const IconX = makeIcon("X", <path d="M18 6 6 18M6 6l12 12" />);
export const IconCheck = makeIcon("Check", <path d="m20 6-11 11-5-5" />);
export const IconChevronDown = makeIcon("ChevronDown", <path d="m6 9 6 6 6-6" />);
export const IconChevronUp = makeIcon("ChevronUp", <path d="m18 15-6-6-6 6" />);
export const IconChevronLeft = makeIcon("ChevronLeft", <path d="m15 18-6-6 6-6" />);
export const IconChevronRight = makeIcon("ChevronRight", <path d="m9 18 6-6-6-6" />);
export const IconArrowLeft = makeIcon("ArrowLeft", <path d="M19 12H5M12 19l-7-7 7-7" />);
export const IconMenu = makeIcon("Menu", <path d="M4 6h16M4 12h16M4 18h16" />);
export const IconMoreHorizontal = makeIcon(
  "MoreHorizontal",
  <>
    <circle cx="5" cy="12" r="1" />
    <circle cx="12" cy="12" r="1" />
    <circle cx="19" cy="12" r="1" />
  </>,
);
export const IconMoreVertical = makeIcon(
  "MoreVertical",
  <>
    <circle cx="12" cy="5" r="1" />
    <circle cx="12" cy="12" r="1" />
    <circle cx="12" cy="19" r="1" />
  </>,
);
export const IconSearch = makeIcon(
  "Search",
  <>
    <circle cx="11" cy="11" r="7" />
    <path d="m21 21-4.3-4.3" />
  </>,
);
export const IconRefresh = makeIcon(
  "Refresh",
  <>
    <path d="M21 12a9 9 0 0 1-15.5 6.2L3 16" />
    <path d="M3 12a9 9 0 0 1 15.5-6.2L21 8" />
    <path d="M21 3v5h-5M3 21v-5h5" />
  </>,
);
export const IconPlay = makeIcon("Play", <path d="M7 4v16l13-8z" />);
export const IconStop = makeIcon("Stop", <rect x="6" y="6" width="12" height="12" rx="1.5" />);
export const IconPower = makeIcon(
  "Power",
  <>
    <path d="M12 3v9" />
    <path d="M18.4 6.6a9 9 0 1 1-12.8 0" />
  </>,
);
export const IconTrash = makeIcon(
  "Trash",
  <>
    <path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2" />
    <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6M10 11v6M14 11v6" />
  </>,
);
export const IconEdit = makeIcon(
  "Edit",
  <>
    <path d="M12 20h9" />
    <path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z" />
  </>,
);
export const IconCopy = makeIcon(
  "Copy",
  <>
    <rect x="9" y="9" width="12" height="12" rx="2" />
    <path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1" />
  </>,
);
export const IconExternalLink = makeIcon(
  "ExternalLink",
  <>
    <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
    <path d="M15 3h6v6M10 14 21 3" />
  </>,
);
export const IconMaximize = makeIcon(
  "Maximize",
  <path d="M8 3H5a2 2 0 0 0-2 2v3M21 8V5a2 2 0 0 0-2-2h-3M3 16v3a2 2 0 0 0 2 2h3M16 21h3a2 2 0 0 0 2-2v-3" />,
);
export const IconMinimize = makeIcon(
  "Minimize",
  <path d="M8 3v3a2 2 0 0 1-2 2H3M21 8h-3a2 2 0 0 1-2-2V3M3 16h3a2 2 0 0 1 2 2v3M16 21v-3a2 2 0 0 1 2-2h3" />,
);
export const IconMonitor = makeIcon(
  "Monitor",
  <>
    <rect x="2" y="3" width="20" height="14" rx="2" />
    <path d="M8 21h8M12 17v4" />
  </>,
);
export const IconGlobe = makeIcon(
  "Globe",
  <>
    <circle cx="12" cy="12" r="10" />
    <path d="M2 12h20M12 2a15 15 0 0 1 0 20M12 2a15 15 0 0 0 0 20" />
  </>,
);
export const IconLayers = makeIcon(
  "Layers",
  <>
    <path d="m12 2 10 5-10 5L2 7z" />
    <path d="m2 17 10 5 10-5M2 12l10 5 10-5" />
  </>,
);
export const IconGrid = makeIcon(
  "Grid",
  <>
    <rect x="3" y="3" width="7" height="7" rx="1" />
    <rect x="14" y="3" width="7" height="7" rx="1" />
    <rect x="3" y="14" width="7" height="7" rx="1" />
    <rect x="14" y="14" width="7" height="7" rx="1" />
  </>,
);
export const IconDatabase = makeIcon(
  "Database",
  <>
    <ellipse cx="12" cy="5" rx="9" ry="3" />
    <path d="M3 5v14c0 1.7 4 3 9 3s9-1.3 9-3V5" />
    <path d="M3 12c0 1.7 4 3 9 3s9-1.3 9-3" />
  </>,
);
export const IconHardDrive = makeIcon(
  "HardDrive",
  <>
    <path d="M22 12H2" />
    <path d="M5.5 5.1 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.5-6.9A2 2 0 0 0 16.8 4H7.2a2 2 0 0 0-1.7 1.1z" />
    <path d="M6 16h.01M10 16h.01" />
  </>,
);
export const IconShield = makeIcon("Shield", <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />);
export const IconUser = makeIcon(
  "User",
  <>
    <circle cx="12" cy="8" r="4" />
    <path d="M4 21a8 8 0 0 1 16 0" />
  </>,
);
export const IconUsers = makeIcon(
  "Users",
  <>
    <circle cx="9" cy="8" r="4" />
    <path d="M1 21a8 8 0 0 1 16 0M17 4a4 4 0 0 1 0 8M23 21a8 8 0 0 0-5-7.4" />
  </>,
);
export const IconSettings = makeIcon(
  "Settings",
  <>
    <circle cx="12" cy="12" r="3" />
    <path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z" />
  </>,
);
export const IconLogOut = makeIcon(
  "LogOut",
  <>
    <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
    <path d="m16 17 5-5-5-5M21 12H9" />
  </>,
);
export const IconSun = makeIcon(
  "Sun",
  <>
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
  </>,
);
export const IconMoon = makeIcon("Moon", <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />);
export const IconLaptop = makeIcon(
  "Laptop",
  <>
    <rect x="4" y="4" width="16" height="11" rx="1.5" />
    <path d="M2 19h20" />
  </>,
);
export const IconInfo = makeIcon(
  "Info",
  <>
    <circle cx="12" cy="12" r="10" />
    <path d="M12 16v-4M12 8h.01" />
  </>,
);
export const IconAlertTriangle = makeIcon(
  "AlertTriangle",
  <>
    <path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z" />
    <path d="M12 9v4M12 17h.01" />
  </>,
);
export const IconAlertCircle = makeIcon(
  "AlertCircle",
  <>
    <circle cx="12" cy="12" r="10" />
    <path d="M12 8v4M12 16h.01" />
  </>,
);
export const IconCheckCircle = makeIcon(
  "CheckCircle",
  <>
    <circle cx="12" cy="12" r="10" />
    <path d="m8 12 3 3 5-6" />
  </>,
);
export const IconClock = makeIcon(
  "Clock",
  <>
    <circle cx="12" cy="12" r="10" />
    <path d="M12 6v6l4 2" />
  </>,
);
export const IconCpu = makeIcon(
  "Cpu",
  <>
    <rect x="5" y="5" width="14" height="14" rx="2" />
    <rect x="9" y="9" width="6" height="6" />
    <path d="M9 1v4M15 1v4M9 19v4M15 19v4M1 9h4M1 15h4M19 9h4M19 15h4" />
  </>,
);
export const IconKey = makeIcon(
  "Key",
  <>
    <circle cx="7.5" cy="15.5" r="4.5" />
    <path d="m10.7 12.3 9.8-9.8M17 6l3 3M14 9l2 2" />
  </>,
);
export const IconLock = makeIcon(
  "Lock",
  <>
    <rect x="4" y="11" width="16" height="10" rx="2" />
    <path d="M8 11V7a4 4 0 0 1 8 0v4" />
  </>,
);
export const IconClipboard = makeIcon(
  "Clipboard",
  <>
    <rect x="8" y="2" width="8" height="4" rx="1" />
    <path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" />
  </>,
);
export const IconActivity = makeIcon("Activity", <path d="M22 12h-4l-3 9L9 3l-3 9H2" />);
export const IconInbox = makeIcon(
  "Inbox",
  <>
    <path d="M22 12h-6l-2 3h-4l-2-3H2" />
    <path d="M5.5 5.1 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.5-6.9A2 2 0 0 0 16.8 4H7.2a2 2 0 0 0-1.7 1.1z" />
  </>,
);
export const IconBox = makeIcon(
  "Box",
  <>
    <path d="M21 8 12 3 3 8v8l9 5 9-5z" />
    <path d="m3 8 9 5 9-5M12 13v8" />
  </>,
);

/** TinyCDI product mark (decorative). */
export function LogoMark({ size = 24, className }: { size?: IconSize; className?: string }) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 32 32"
      width={size}
      height={size}
      aria-hidden="true"
      focusable="false"
      className={cx("tc-logo-mark", className)}
    >
      <rect x="1" y="1" width="30" height="30" rx="8" className="tc-logo-mark__bg" />
      <rect x="7" y="9" width="18" height="12" rx="2" className="tc-logo-mark__screen" />
      <path d="M12 25h8" className="tc-logo-mark__stand" />
    </svg>
  );
}
