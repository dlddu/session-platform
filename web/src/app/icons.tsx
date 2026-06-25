// Inline SVG icons ported 1:1 from docs/mockups/index.html (ICON map + crystal).
// Kept as small components so cards/chips can size them via the `size` prop.

type IconProps = { size?: number; className?: string };

export function PodIcon({ size = 12, className }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
    >
      <path d="M12 2 21 7v10l-9 5-9-5V7l9-5Z" />
    </svg>
  );
}

export function ClockIcon({ size = 13, className }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
    >
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3 2" />
    </svg>
  );
}

export function PinIcon({ size = 13, className }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
    >
      <path d="M12 21s7-6.3 7-11a7 7 0 0 0-14 0c0 4.7 7 11 7 11Z" />
      <circle cx="12" cy="10" r="2.5" />
    </svg>
  );
}

// The frosted snapshot crystal (snap-body glyph).
export function CrystalIcon({ size = 44, className }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
    >
      <path d="M12 2v20M12 2 9.5 4.5M12 2l2.5 2.5M12 22l-2.5-2.5M12 22l2.5-2.5M3.3 7l17.4 10M3.3 7l3.4.9M3.3 7l.9 3.4M20.7 17l-3.4-.9M20.7 17l-.9-3.4M20.7 7 3.3 17M20.7 7l-3.4.9M20.7 7l-.9 3.4M3.3 17l3.4-.9M3.3 17l.9-3.4" />
    </svg>
  );
}
