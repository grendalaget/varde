export default function Cairn({
  className = "h-6 w-6",
}: {
  className?: string;
}) {
  return (
    <svg viewBox="0 0 24 24" className={className} aria-hidden>
      <ellipse cx="12" cy="20" rx="8" ry="2.6" fill="#34d399" />
      <ellipse cx="12" cy="14.6" rx="6" ry="2.4" fill="#10b981" />
      <ellipse cx="12" cy="9.6" rx="4.2" ry="2.1" fill="#059669" />
      <ellipse cx="12" cy="5.4" rx="2.6" ry="1.7" fill="#047857" />
    </svg>
  );
}
