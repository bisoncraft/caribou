git log --numstat --format='%aN' a4af879cd..HEAD | awk '
NF == 1 { author = $1 }
NF == 3 { added[author] += $1; removed[author] += $2 }
END {
  for (author in added)
    printf "%s: +%d, -%d, total: %d\n", author, added[author], removed[author], added[author] - removed[author]
}'
