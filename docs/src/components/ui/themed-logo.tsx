"use client";

export function ThemedLogo() {
  return (
    <div className="relative flex items-center justify-center size-8">
      <svg
        viewBox="0 0 32 32"
        fill="none"
        xmlns="http://www.w3.org/2000/svg"
        className="size-8"
        aria-hidden="true"
      >
        {/* Brain / neural network symbol */}
        <rect
          x="2"
          y="2"
          width="28"
          height="28"
          rx="6"
          className="fill-violet-500 dark:fill-violet-400"
        />
        {/* Central node */}
        <circle cx="16" cy="16" r="3" className="fill-white" />
        {/* Satellite nodes */}
        <circle cx="9" cy="10" r="2" className="fill-white/80" />
        <circle cx="23" cy="10" r="2" className="fill-white/80" />
        <circle cx="9" cy="22" r="2" className="fill-white/80" />
        <circle cx="23" cy="22" r="2" className="fill-white/80" />
        {/* Connections */}
        <line
          x1="12"
          y1="12"
          x2="14"
          y2="14"
          stroke="white"
          strokeWidth="1.2"
          strokeOpacity="0.6"
        />
        <line
          x1="20"
          y1="12"
          x2="18"
          y2="14"
          stroke="white"
          strokeWidth="1.2"
          strokeOpacity="0.6"
        />
        <line
          x1="12"
          y1="20"
          x2="14"
          y2="18"
          stroke="white"
          strokeWidth="1.2"
          strokeOpacity="0.6"
        />
        <line
          x1="20"
          y1="20"
          x2="18"
          y2="18"
          stroke="white"
          strokeWidth="1.2"
          strokeOpacity="0.6"
        />
      </svg>
    </div>
  );
}
