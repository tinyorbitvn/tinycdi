import type { ReactNode, SVGProps } from "react";
import { cx } from "./cx";

// Orbit 2.9 icon set — Tabler Icons 3.48.0 outline artwork (@tabler/icons,
// MIT; license in ./orbit/LICENSES/LICENSE-Tabler.txt) at the manifest
// names, on the shared 24×24 grid. Orbit normalizes Tabler's 2px stroke to
// 1.75px via makeIcon; geometry is verbatim from the upstream SVGs.
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


export const IconPlus = makeIcon(
  "Plus",
  <>
    <path d="M12 5l0 14" />
    <path d="M5 12l14 0" />
  </>,
);

export const IconX = makeIcon(
  "X",
  <>
    <path d="M18 6l-12 12" />
    <path d="M6 6l12 12" />
  </>,
);

export const IconCheck = makeIcon("Check", <path d="M5 12l5 5l10 -10" />);

export const IconChevronDown = makeIcon("ChevronDown", <path d="M6 9l6 6l6 -6" />);

export const IconChevronUp = makeIcon("ChevronUp", <path d="M6 15l6 -6l6 6" />);

export const IconChevronLeft = makeIcon("ChevronLeft", <path d="M15 6l-6 6l6 6" />);

export const IconChevronRight = makeIcon("ChevronRight", <path d="M9 6l6 6l-6 6" />);

export const IconSelector = makeIcon(
  "Selector",
  <>
    <path d="M8 9l4 -4l4 4" />
    <path d="M16 15l-4 4l-4 -4" />
  </>,
);

export const IconArrowLeft = makeIcon(
  "ArrowLeft",
  <>
    <path d="M5 12l14 0" />
    <path d="M5 12l6 6" />
    <path d="M5 12l6 -6" />
  </>,
);

export const IconMenu = makeIcon(
  "Menu",
  <>
    <path d="M4 6l16 0" />
    <path d="M4 12l16 0" />
    <path d="M4 18l16 0" />
  </>,
);

export const IconDots = makeIcon(
  "Dots",
  <>
    <path d="M4 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
    <path d="M11 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
    <path d="M18 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
  </>,
);

export const IconDotsVertical = makeIcon(
  "DotsVertical",
  <>
    <path d="M11 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
    <path d="M11 19a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
    <path d="M11 5a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
  </>,
);

export const IconSearch = makeIcon(
  "Search",
  <>
    <path d="M3 10a7 7 0 1 0 14 0a7 7 0 1 0 -14 0" />
    <path d="M21 21l-6 -6" />
  </>,
);

export const IconRefresh = makeIcon(
  "Refresh",
  <>
    <path d="M20 11a8.1 8.1 0 0 0 -15.5 -2m-.5 -4v4h4" />
    <path d="M4 13a8.1 8.1 0 0 0 15.5 2m.5 4v-4h-4" />
  </>,
);

export const IconExternalLink = makeIcon(
  "ExternalLink",
  <>
    <path d="M12 6h-6a2 2 0 0 0 -2 2v10a2 2 0 0 0 2 2h10a2 2 0 0 0 2 -2v-6" />
    <path d="M11 13l9 -9" />
    <path d="M15 4h5v5" />
  </>,
);

export const IconMaximize = makeIcon(
  "Maximize",
  <>
    <path d="M4 8v-2a2 2 0 0 1 2 -2h2" />
    <path d="M4 16v2a2 2 0 0 0 2 2h2" />
    <path d="M16 4h2a2 2 0 0 1 2 2v2" />
    <path d="M16 20h2a2 2 0 0 0 2 -2v-2" />
  </>,
);

export const IconMinimize = makeIcon(
  "Minimize",
  <>
    <path d="M15 19v-2a2 2 0 0 1 2 -2h2" />
    <path d="M15 5v2a2 2 0 0 0 2 2h2" />
    <path d="M5 15h2a2 2 0 0 1 2 2v2" />
    <path d="M5 9h2a2 2 0 0 0 2 -2v-2" />
  </>,
);

export const IconDownload = makeIcon(
  "Download",
  <>
    <path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2 -2v-2" />
    <path d="M7 11l5 5l5 -5" />
    <path d="M12 4l0 12" />
  </>,
);

export const IconFilter = makeIcon("Filter", <path d="M4 4h16v2.172a2 2 0 0 1 -.586 1.414l-4.414 4.414v7l-6 2v-8.5l-4.48 -4.928a2 2 0 0 1 -.52 -1.345v-2.227" />);

export const IconLayoutDashboard = makeIcon(
  "LayoutDashboard",
  <>
    <path d="M5 4h4a1 1 0 0 1 1 1v6a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1v-6a1 1 0 0 1 1 -1" />
    <path d="M5 16h4a1 1 0 0 1 1 1v2a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1v-2a1 1 0 0 1 1 -1" />
    <path d="M15 12h4a1 1 0 0 1 1 1v6a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1v-6a1 1 0 0 1 1 -1" />
    <path d="M15 4h4a1 1 0 0 1 1 1v2a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1v-2a1 1 0 0 1 1 -1" />
  </>,
);

export const IconLayoutGrid = makeIcon(
  "LayoutGrid",
  <>
    <path d="M4 5a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4" />
    <path d="M14 5a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4" />
    <path d="M4 15a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4" />
    <path d="M14 15a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4" />
  </>,
);

export const IconPlayerPlay = makeIcon("PlayerPlay", <path d="M7 4v16l13 -8l-13 -8" />);

export const IconPlayerStop = makeIcon("PlayerStop", <path d="M5 7a2 2 0 0 1 2 -2h10a2 2 0 0 1 2 2v10a2 2 0 0 1 -2 2h-10a2 2 0 0 1 -2 -2l0 -10" />);

export const IconPower = makeIcon(
  "Power",
  <>
    <path d="M7 6a7.75 7.75 0 1 0 10 0" />
    <path d="M12 4l0 8" />
  </>,
);

export const IconTrash = makeIcon(
  "Trash",
  <>
    <path d="M4 7l16 0" />
    <path d="M10 11l0 6" />
    <path d="M14 11l0 6" />
    <path d="M5 7l1 12a2 2 0 0 0 2 2h8a2 2 0 0 0 2 -2l1 -12" />
    <path d="M9 7v-3a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v3" />
  </>,
);

export const IconEdit = makeIcon(
  "Edit",
  <>
    <path d="M7 7h-1a2 2 0 0 0 -2 2v9a2 2 0 0 0 2 2h9a2 2 0 0 0 2 -2v-1" />
    <path d="M20.385 6.585a2.1 2.1 0 0 0 -2.97 -2.97l-8.415 8.385v3h3l8.385 -8.415" />
    <path d="M16 5l3 3" />
  </>,
);

export const IconClipboard = makeIcon(
  "Clipboard",
  <>
    <path d="M9 5h-2a2 2 0 0 0 -2 2v12a2 2 0 0 0 2 2h10a2 2 0 0 0 2 -2v-12a2 2 0 0 0 -2 -2h-2" />
    <path d="M9 5a2 2 0 0 1 2 -2h2a2 2 0 0 1 2 2a2 2 0 0 1 -2 2h-2a2 2 0 0 1 -2 -2" />
  </>,
);

export const IconInbox = makeIcon(
  "Inbox",
  <>
    <path d="M4 6a2 2 0 0 1 2 -2h12a2 2 0 0 1 2 2v12a2 2 0 0 1 -2 2h-12a2 2 0 0 1 -2 -2l0 -12" />
    <path d="M4 13h3l3 3h4l3 -3h3" />
  </>,
);

export const IconEye = makeIcon(
  "Eye",
  <>
    <path d="M10 12a2 2 0 1 0 4 0a2 2 0 0 0 -4 0" />
    <path d="M21 12c-2.4 4 -5.4 6 -9 6c-3.6 0 -6.6 -2 -9 -6c2.4 -4 5.4 -6 9 -6c3.6 0 6.6 2 9 6" />
  </>,
);

export const IconHistory = makeIcon(
  "History",
  <>
    <path d="M12 8l0 4l2 2" />
    <path d="M3.05 11a9 9 0 1 1 .5 4m-.5 5v-5h5" />
  </>,
);

export const IconFileText = makeIcon(
  "FileText",
  <>
    <path d="M14 3v4a1 1 0 0 0 1 1h4" />
    <path d="M17 21h-10a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h7l5 5v11a2 2 0 0 1 -2 2" />
    <path d="M9 9l1 0" />
    <path d="M9 13l6 0" />
    <path d="M9 17l6 0" />
  </>,
);

export const IconBell = makeIcon(
  "Bell",
  <>
    <path d="M10 5a2 2 0 1 1 4 0a7 7 0 0 1 4 6v3a4 4 0 0 0 2 3h-16a4 4 0 0 0 2 -3v-3a7 7 0 0 1 4 -6" />
    <path d="M9 17v1a3 3 0 0 0 6 0v-1" />
  </>,
);

export const IconWorld = makeIcon(
  "World",
  <>
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 0 0 -18 0" />
    <path d="M3.6 9h16.8" />
    <path d="M3.6 15h16.8" />
    <path d="M11.5 3a17 17 0 0 0 0 18" />
    <path d="M12.5 3a17 17 0 0 1 0 18" />
  </>,
);

export const IconDatabase = makeIcon(
  "Database",
  <>
    <path d="M4 6a8 3 0 1 0 16 0a8 3 0 1 0 -16 0" />
    <path d="M4 6v6a8 3 0 0 0 16 0v-6" />
    <path d="M4 12v6a8 3 0 0 0 16 0v-6" />
  </>,
);

export const IconServer = makeIcon(
  "Server",
  <>
    <path d="M3 7a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3v-2" />
    <path d="M3 15a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3l0 -2" />
    <path d="M7 8l0 .01" />
    <path d="M7 16l0 .01" />
  </>,
);

export const IconContainer = makeIcon(
  "Container",
  <>
    <path d="M20 4v.01" />
    <path d="M20 20v.01" />
    <path d="M20 16v.01" />
    <path d="M20 12v.01" />
    <path d="M20 8v.01" />
    <path d="M8 5a1 1 0 0 1 1 -1h6a1 1 0 0 1 1 1v14a1 1 0 0 1 -1 1h-6a1 1 0 0 1 -1 -1l0 -14" />
    <path d="M4 4v.01" />
    <path d="M4 20v.01" />
    <path d="M4 16v.01" />
    <path d="M4 12v.01" />
    <path d="M4 8v.01" />
  </>,
);

export const IconChart = makeIcon(
  "Chart",
  <>
    <path d="M3 13a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v6a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -6" />
    <path d="M15 9a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v10a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -10" />
    <path d="M9 5a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v14a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -14" />
    <path d="M4 20h14" />
  </>,
);

export const IconChartLine = makeIcon(
  "ChartLine",
  <>
    <path d="M4 19l16 0" />
    <path d="M4 15l4 -6l4 2l4 -5l4 4" />
  </>,
);

export const IconGauge = makeIcon(
  "Gauge",
  <>
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 1 0 -18 0" />
    <path d="M11 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
    <path d="M13.41 10.59l2.59 -2.59" />
    <path d="M7 12a5 5 0 0 1 5 -5" />
  </>,
);

export const IconUser = makeIcon(
  "User",
  <>
    <path d="M8 7a4 4 0 1 0 8 0a4 4 0 0 0 -8 0" />
    <path d="M6 21v-2a4 4 0 0 1 4 -4h4a4 4 0 0 1 4 4v2" />
  </>,
);

export const IconUsers = makeIcon(
  "Users",
  <>
    <path d="M5 7a4 4 0 1 0 8 0a4 4 0 1 0 -8 0" />
    <path d="M3 21v-2a4 4 0 0 1 4 -4h4a4 4 0 0 1 4 4v2" />
    <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    <path d="M21 21v-2a4 4 0 0 0 -3 -3.85" />
  </>,
);

export const IconUserCheck = makeIcon(
  "UserCheck",
  <>
    <path d="M8 7a4 4 0 1 0 8 0a4 4 0 0 0 -8 0" />
    <path d="M6 21v-2a4 4 0 0 1 4 -4h4" />
    <path d="M15 19l2 2l4 -4" />
  </>,
);

export const IconShieldCheck = makeIcon(
  "ShieldCheck",
  <>
    <path d="M11.46 20.846a12 12 0 0 1 -7.96 -14.846a12 12 0 0 0 8.5 -3a12 12 0 0 0 8.5 3a12 12 0 0 1 -.09 7.06" />
    <path d="M15 19l2 2l4 -4" />
  </>,
);

export const IconLogout = makeIcon(
  "Logout",
  <>
    <path d="M14 8v-2a2 2 0 0 0 -2 -2h-7a2 2 0 0 0 -2 2v12a2 2 0 0 0 2 2h7a2 2 0 0 0 2 -2v-2" />
    <path d="M9 12h12l-3 -3" />
    <path d="M18 15l3 -3" />
  </>,
);

export const IconSettings = makeIcon(
  "Settings",
  <>
    <path d="M10.325 4.317c.426 -1.756 2.924 -1.756 3.35 0a1.724 1.724 0 0 0 2.573 1.066c1.543 -.94 3.31 .826 2.37 2.37a1.724 1.724 0 0 0 1.065 2.572c1.756 .426 1.756 2.924 0 3.35a1.724 1.724 0 0 0 -1.066 2.573c.94 1.543 -.826 3.31 -2.37 2.37a1.724 1.724 0 0 0 -2.572 1.065c-.426 1.756 -2.924 1.756 -3.35 0a1.724 1.724 0 0 0 -2.573 -1.066c-1.543 .94 -3.31 -.826 -2.37 -2.37a1.724 1.724 0 0 0 -1.065 -2.572c-1.756 -.426 -1.756 -2.924 0 -3.35a1.724 1.724 0 0 0 1.066 -2.573c-.94 -1.543 .826 -3.31 2.37 -2.37c1 .608 2.296 .07 2.572 -1.065" />
    <path d="M9 12a3 3 0 1 0 6 0a3 3 0 0 0 -6 0" />
  </>,
);

export const IconInfo = makeIcon(
  "Info",
  <>
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 0 0 -18 0" />
    <path d="M12 9h.01" />
    <path d="M11 12h1v4h1" />
  </>,
);

export const IconTriangleAlert = makeIcon(
  "TriangleAlert",
  <>
    <path d="M12 9v4" />
    <path d="M10.363 3.591l-8.106 13.534a1.914 1.914 0 0 0 1.636 2.871h16.214a1.914 1.914 0 0 0 1.636 -2.87l-8.106 -13.536a1.914 1.914 0 0 0 -3.274 0" />
    <path d="M12 16h.01" />
  </>,
);

export const IconCircleAlert = makeIcon(
  "CircleAlert",
  <>
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 0 0 -18 0" />
    <path d="M12 8v4" />
    <path d="M12 16h.01" />
  </>,
);

export const IconCircleCheck = makeIcon(
  "CircleCheck",
  <>
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 1 0 -18 0" />
    <path d="M9 12l2 2l4 -4" />
  </>,
);

export const IconCircle = makeIcon("Circle", <path d="M3 12a9 9 0 1 0 18 0a9 9 0 1 0 -18 0" />);

export const IconCircleDot = makeIcon(
  "CircleDot",
  <>
    <path d="M11 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0" />
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 1 0 -18 0" />
  </>,
);

export const IconCircleMinus = makeIcon(
  "CircleMinus",
  <>
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 1 0 -18 0" />
    <path d="M9 12l6 0" />
  </>,
);

export const IconClock = makeIcon(
  "Clock",
  <>
    <path d="M3 12a9 9 0 1 0 18 0a9 9 0 0 0 -18 0" />
    <path d="M12 7v5l3 3" />
  </>,
);

export const IconSun = makeIcon(
  "Sun",
  <>
    <path d="M8 12a4 4 0 1 0 8 0a4 4 0 1 0 -8 0" />
    <path d="M3 12h1m8 -9v1m8 8h1m-9 8v1m-6.4 -15.4l.7 .7m12.1 -.7l-.7 .7m0 11.4l.7 .7m-12.1 -.7l-.7 .7" />
  </>,
);

export const IconMoon = makeIcon("Moon", <path d="M12 3c.132 0 .263 0 .393 0a7.5 7.5 0 0 0 7.92 12.446a9 9 0 1 1 -8.313 -12.454l0 .008" />);

export const IconDeviceDesktop = makeIcon(
  "DeviceDesktop",
  <>
    <path d="M3 5a1 1 0 0 1 1 -1h16a1 1 0 0 1 1 1v10a1 1 0 0 1 -1 1h-16a1 1 0 0 1 -1 -1v-10" />
    <path d="M7 20h10" />
    <path d="M9 16v4" />
    <path d="M15 16v4" />
  </>,
);

export const IconDeviceLaptop = makeIcon(
  "DeviceLaptop",
  <>
    <path d="M3 19l18 0" />
    <path d="M5 7a1 1 0 0 1 1 -1h12a1 1 0 0 1 1 1v8a1 1 0 0 1 -1 1h-12a1 1 0 0 1 -1 -1l0 -8" />
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
