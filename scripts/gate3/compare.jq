# Compare what capture A's spot-vol path predicted for the move to capture B with
# what the broker's M2M actually did between them.
# Usage: jq -n --slurpfile a A.json --slurpfile b B.json -f scripts/gate3/compare.jq
$a[0] as $A | $b[0] as $B
| (($B.ref_px / $A.ref_px - 1) * 100) as $s
| $A.steps as $x | $A.book.grid.path.mid as $y
| ([range(0; ($x | length) - 1)] | map(select($x[.] <= $s and $s <= $x[. + 1])) | first) as $i
| (if $i == null then null
   else $y[$i] + ($y[$i + 1] - $y[$i]) * ($s - $x[$i]) / ($x[$i + 1] - $x[$i]) end) as $spot
| (($B.ts - $A.ts) / 86400000) as $days
| ($A.book.t * $days) as $theta
| {
    from: ($A.ts / 1000 | strflocaltime("%H:%M:%S")), to: ($B.ts / 1000 | strflocaltime("%H:%M:%S")),
    broker_age_s: [(($A.ts - $A.book_ts) / 1000 | round), (($B.ts - $B.book_ts) / 1000 | round)],
    same_legs: (($A.legs | map([.id, .q])) == ($B.legs | map([.id, .q]))),
    flagged: (($A.book.flags | length) + ($B.book.flags | length) + ($A.unmod | length) + ($B.unmod | length)),
    move_pct: ($s * 1000 | round / 1000),
    pred_spot_vol: (if $spot == null then null else ($spot | round) end), pred_theta: ($theta | round),
    pred: (if $spot == null then null else ($spot + $theta | round) end),
    broker: ($B.pnl.bm2m - $A.pnl.bm2m | round),
    terminal: ($B.pnl.m2m - $A.pnl.m2m | round)
  }
| .err = (if .pred == null then null else .pred - .broker end)
| .err_pct = (if .err == null or .broker == 0 then null else (.err / (.broker | fabs) * 1000 | round / 10) end)
