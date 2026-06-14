// badgeAbbrev derives a short, toolbar-sized label from a proxy display name.
//
// Canonical PIO display names follow the pattern {label}-{CC}-{NN} where CC is
// a 2-letter country code (e.g. "share-DE-01" → "DE", "dedicated-US-01" → "US").
// We extract segment[1] (after the first '-') as the badge text. For names with
// no '-', or where segment[1] is blank (e.g. "a--b"), we fall back to the old
// compact-first-3 abbreviation. The badge is never blank while a proxy is active:
// if both paths yield nothing, we return 'ON'.
export function badgeAbbrev(name) {
  const s = String(name || '');
  const dash = s.indexOf('-');
  if (dash !== -1) {
    const seg = s.split('-')[1].trim();
    if (seg.length > 0) return seg.toUpperCase().slice(0, 4);
  }
  // Fallback: compact first 3 letters/digits (Unicode-aware), or 'ON'.
  const compact = s.replace(/[^\p{L}\p{N}]/gu, '').toUpperCase();
  return compact.slice(0, 3) || 'ON';
}
