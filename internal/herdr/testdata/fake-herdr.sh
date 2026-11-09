#!/bin/sh
# Records its arguments (one per line, then a separator) and answers like herdr.
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_HERDR_LOG"; done
echo '--' >> "$FAKE_HERDR_LOG"
case "$1 $2" in
  "tab create") echo '{"id":"x","result":{"tab":{"tab_id":"w1:t9"},"root_pane":{"pane_id":"w1:p42"}}}' ;;
  "pane split") echo '{"id":"x","result":{"pane":{"pane_id":"w1:p43"}}}' ;;
  "pane run")   echo '{"id":"x","result":{}}' ;;
  *) echo '{"error":{"code":"unknown","message":"bad"}}'; exit 1 ;;
esac
