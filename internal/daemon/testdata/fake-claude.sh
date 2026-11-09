#!/bin/sh
# Stand-in for `claude -p` used by the daemon tests. It plays the agent's side
# of the protocol by writing the same rows the taskpilot MCP tools write.
set -eu
dir="$TASKPILOT_HOME/tasks/$TASKPILOT_TASK_ID"
mkdir -p "$dir"
db="$TASKPILOT_HOME/taskpilot.db"
prompt=$(cat)
printf "%s" "$*" | tr "\n" " " >> "$dir/fake-args.log"; echo >> "$dir/fake-args.log"

sid=""
prev=""
for a in "$@"; do
  case "$prev" in --session-id|--resume) sid="$a" ;; esac
  prev="$a"
done

sql() { # statement, args...
  python3 -I -c 'import sqlite3, sys, datetime
db = sqlite3.connect(sys.argv[1], timeout=10)
args = [datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ") if a == "NOW" else a for a in sys.argv[3:]]
db.execute(sys.argv[2], args); db.commit()' "$db" "$@"
}
request() { # id gate attempt
  sql "INSERT INTO approvals (task_id, id, gate, attempt, summary, created_at) VALUES (?,?,?,?,?,?)" \
    "$TASKPILOT_TASK_ID" "$1" "$2" "$3" "fake $2" NOW
}

case "$prompt" in
  *"SLOW-AFTER-GATE"*) request plan-1 plan 1; sleep 30 & wait ;;
  *"SLOW"*) sleep 30 & wait ;;
  *"REJECTED request plan-1"*) request plan-2 plan 2 ;;
  *"APPROVED request plan-"*) request pr-1 pr 1 ;;
  *"APPROVED request pr-1"*)
    sql "INSERT INTO completions (task_id, summary, pr_url, created_at) VALUES (?,?,?,?)" \
      "$TASKPILOT_TASK_ID" "fake done" "https://example/pr/1" NOW ;;
  *"Message from the operator"*) : ;;   # ends without a gate -> needs_attention
  *) request plan-1 plan 1 ;;
esac

printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$sid"
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","id":"tu1","name":"Edit","input":{"file_path":"a.go"}},{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"git push origin x"}}]},"session_id":"%s"}\n' "$sid"
printf '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu2","is_error":true,"content":"PreToolUse:Bash hook error: taskpilot gate \\"pr\\" is not approved yet"}]},"session_id":"%s"}\n' "$sid"
printf '{"type":"result","subtype":"success","is_error":false,"result":"turn done","total_cost_usd":0.25,"num_turns":3,"usage":{"input_tokens":100,"output_tokens":20},"session_id":"%s"}\n' "$sid"
