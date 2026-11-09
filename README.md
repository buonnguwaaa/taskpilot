# taskpilot

Điều phối các task lấy từ ticket qua Claude Code chạy headless. Operator không gõ
prompt nữa: chỉ **duyệt ở các cổng** (plan, PR, deploy) trong một TUI riêng.

## Thiết kế

```
┌──────────── taskpilot tui (Bubble Tea) ────────────┐
│ danh sách task │ chi tiết · yêu cầu duyệt · activity│
└───────────────────────┬────────────────────────────┘
                        │  đọc trạng thái, ghi quyết định (file)
        taskpilot daemon (harness — không có logic model)
   lấy ticket → git worktree → chạy 1 lượt claude -p → đỗ ở cổng
   → chờ quyết định → claude -p --resume <cùng session>
                        │
     1 session Claude Code / task (một context xuyên suốt)
     skill  taskpilot:workflow   (thứ tự công việc)
     MCP    taskpilot            (request_approval, complete_task, task_info)
     hook   PreToolUse           (ép cổng: Edit trước plan, push trước pr, ...)
     MCP    graphify / argocd / ... theo từng repo
```

Các nguyên tắc chính, theo bài [When to fold subagents into your main agent loop](https://claude.com/resources/articles/building-agents-when-to-fold-subagents-into-your-main-agent-loop):

- **Một vòng agent cho mỗi task**, không chia thành các subagent planner, coder,
  reviewer. Plan, code và verify dùng chung một context, nên không mất thông tin
  qua các lần chuyển giao.
- **Thứ tự công việc nằm trong skill**, còn **ràng buộc cứng nằm trong harness**.
  Hook chặn tool call vượt cổng, bất kể model quyết định gì.
- **Daemon chỉ là code điều phối**: nó tạo worktree, chạy rồi tiếp tục session,
  lưu trạng thái và báo cáo về ticket.

### Trạng thái task

```
queued → preparing → running ─┬─> awaiting_approval ──(approve/reject)──> queued (resume)
                              ├─> done
                              └─> needs_attention ──(message)──> queued (resume)
```

Mỗi file trạng thái chỉ có **một bên ghi**, nên không cần khóa:

| File (`<home>/tasks/<id>/`) | Bên ghi | Nội dung |
|---|---|---|
| `task.json` | daemon | bản ghi task, trạng thái, chi phí |
| `requests/<gate>-<n>.json` | MCP (agent) | yêu cầu duyệt |
| `decisions/<gate>-<n>.json` | TUI / CLI | quyết định của operator |
| `completion.json` | MCP (agent) | agent báo đã xong |
| `nudge.json` | TUI / CLI | message để chạy tiếp task đang dừng |
| `takeover` | TUI | operator đang điều khiển session trực tiếp |
| `events.jsonl`, `runs/*.log` | daemon | stream-json và stderr của từng lượt chạy |

### Cổng và chính sách

Mặc định (`config.DefaultRules`):

| Thao tác | Cần cổng |
|---|---|
| `Edit`, `Write`, `MultiEdit`, `NotebookEdit` | `plan` |
| `git push`, `gh pr create/merge` | `pr` |
| `argocd app sync/rollback/...` (CLI hoặc MCP) | `deploy` |
| `vault read`, `vault kv get/put`, `vault write`, force-push | **luôn bị chặn** |

Ngoài các cổng, `--permission-mode dontAsk` và `--permission-prompts none` đảm
bảo chỉ những tool trong allowlist (`allowed_tools` của repo cộng với danh sách
mặc định) mới chạy được. Mọi tool khác bị từ chối mà không hỏi ai.

## Dùng hằng ngày: chỉ cần gõ `taskpilot`

```bash
go install ./cmd/taskpilot   # một lần
taskpilot                    # mở TUI; daemon chạy bên trong, không cần terminal thứ hai
```

| Tab | Làm gì |
|---|---|
| **1 Tasks** | Task của agent. `a` duyệt, `A` duyệt kèm ghi chú, `x` từ chối kèm nhận xét, `m` nhắn agent, `p` mở PR, `o` tự làm trong session của agent, `n` tạo task mới bằng form, `f` chỉ hiện task cần bạn |
| **2 Hasaki Work** | Task của bạn trên Hasaki Work, kèm phạm vi (bom/plm/payment) và mô tả. **Enter** (hoặc click lần hai) mở form giao cho agent: chọn project, repo bắt đầu, ghi chú. `s` đổi bộ lọc |
| **3 Logs** | Bấm `e` để đổi môi trường. **QC · Argo CD** (cả bom, payment, plm): danh sách app kèm health và sync, revision, image; Enter xem log pod, `/` lọc, `t` khoảng thời gian, `o` mở trên Argo CD, `S` sync (hỏi xác nhận). **Production · Splunk** (bom, payment; PLM không có log prod): truy vấn có sẵn (hỏi tham số), `/` tìm tự do, `t` thời gian, `o` Splunk Web, `c` copy SPL, `i` kiến thức log của project |
| **4 Status** | Đăng nhập (`l` đăng nhập Hasaki Work, `t` dán JWT, `b` lưu token Bitbucket), `d` chạy doctor, log của daemon |

- **Chuột:** click tên tab, click dòng để chọn, cuộn để đọc plan hoặc diff dài.
- **`:`** mở thanh lệnh để gõ bất kỳ lệnh `taskpilot` nào (ví dụ `hw show 14352972`, `doctor`).
  Lệnh cần nhập liệu, như `auth login`, sẽ được nhường terminal để chạy.
- **`?`** xem trợ giúp. **`q`** thoát; nếu còn task đang chạy, TUI hỏi xác nhận trước, vì
  thoát sẽ dừng các task đó.
- Chỉ một daemon được chạy cho mỗi `home` (có file khóa). Mở TUI thứ hai thì TUI
  đó chỉ hiển thị, không lập lịch task.

### Dùng trong herdr (khuyên dùng)

Khi `taskpilot` được mở **bên trong herdr** và chưa có daemon nào, nó tự tạo tab
**"taskpilot daemon"** chạy `taskpilot daemon`. TUI khi đó chỉ làm giao diện
(header ghi "daemon: tab herdr"), nên:

- thoát TUI (`q`, Ctrl+C) **không** dừng task, và không hỏi xác nhận;
- đóng terminal hay mất SSH cũng không sao: `herdr` để gắn lại;
- chỉ dừng daemon khi đóng tab "taskpilot daemon" hoặc server herdr tắt. Khi đó
  task đang chạy chuyển `needs_attention`; bấm `m` để chạy tiếp.

Phím `o` mở session của agent ở **pane herdr bên phải** qua lệnh
`taskpilot takeover TASK`. Lệnh này đánh dấu task đang làm tay (kèm PID, nên
nếu pane bị đóng đột ngột thì dấu cũ không chặn daemon mãi), chạy
`claude --resume`, rồi trả task cho daemon khi bạn thoát.

Muốn daemon chạy bên trong TUI như cũ thì dùng `taskpilot -embedded-daemon`.

## Cài đặt và chạy (các lệnh vẫn dùng được)

```bash
go build -o bin/taskpilot ./cmd/taskpilot
bin/taskpilot init                 # ghi ~/.config/taskpilot/config.yaml, sau đó sửa phần repos
bin/taskpilot daemon               # hoặc: daemon -once (một vòng rồi thoát, dùng được với cron)
bin/taskpilot tui                  # ở một terminal khác
```

Daemon chạy không có người trông, nên nên dùng `claude setup-token` thay cho
phiên đăng nhập tương tác, vì phiên tương tác có thể hết hạn.

### Project và workspace

Mỗi task thuộc **một project** (ví dụ BOM, PLM, payment). Mỗi project gom các repo
của nó, kèm file kiến thức chung (`context_files`) và id project tương ứng trên
Hasaki Work. Khi chạy:

- Mỗi task có một workspace `~/.taskpilot/worktrees/<task>/`, lúc đầu rỗng.
- Agent mở repo cần dùng bằng MCP tool `open_repo`. Repo được tạo thành worktree
  `./<repo>` trên nhánh `tp/<task>`, lấy từ `origin/<base>` vừa fetch. Agent chỉ
  mở được các repo **thuộc project của task**.
- Lần đầu mở một repo, harness chạy lệnh verify trên code gốc và ghi lại
  **baseline**. Nếu nhánh gốc vốn đã fail, agent chỉ cần không làm phát sinh lỗi
  mới.
- Hook chặn mọi thao tác ghi file nằm ngoài workspace. Checkout bạn đang làm việc
  trong `~/repos/...` không bao giờ bị sửa.

`taskpilot doctor` kiểm tra config trên máy: repo có tồn tại không, có
`origin/<base>` không, các công cụ verify có trong PATH không, trạng thái đăng nhập.

Tạo ticket thử (khi dùng mock source):

```bash
taskpilot submit -project bom -title "…" -body "$(taskpilot hw show 14352972)"
taskpilot submit -project bom -repo bom-api -title "…"   # -repo: mở sẵn một repo
```

### Hasaki Work

```bash
taskpilot auth login     # mở Chrome với profile riêng, bạn đăng nhập SSO, phiên được lưu vào DB, rồi Chrome tự đóng
taskpilot auth status    # token và phiên còn bao lâu (không in giá trị)
taskpilot auth set-token # dự phòng: dán JWT bằng tay (không hiện ký tự)
taskpilot hw tasks       # task đang mở của bạn
taskpilot hw show <id>   # chi tiết và mô tả của một task
```

Token được tự lấy lại từ session cookie khi còn dưới 6 giờ. Session cookie hết
hạn sau khoảng 7 ngày; khi đó TUI báo đỏ và bạn chạy lại `auth login`. API được
mô tả ở [docs/hasaki-work-api.md](docs/hasaki-work-api.md).

### Pull request trên Bitbucket: luồng QC rồi mới tới main

Luồng của team, khai báo bằng `pr_flow` trong config (có thể ghi đè cho từng repo):

```
main ──checkout──► tp/<task>
                     ├─ PR ①  → release/qc   (sau khi duyệt cổng pr)  → QC test
                     └─ PR ②  → main         (sau khi duyệt cổng qc), mở từ CHÍNH nhánh feature
main → release/prod: bước release do người làm, không phải việc của agent
```

1. Bạn duyệt `pr`. Agent gọi `create_pull_request` cho từng repo đã sửa: push nhánh
   và mở PR vào `release/qc`. Sau đó agent xin duyệt **`qc`**, kèm checklist test QC.
2. QC báo lỗi: bạn từ chối `qc` và mô tả lỗi. Agent sửa ngay trên nhánh đó; gọi lại
   tool sẽ push commit mới vào PR QC đang mở. Agent xin duyệt `qc` lại.
3. QC đạt: bạn duyệt `qc`. Agent mở PR vào `main` (hoặc `master`, tùy `base`) rồi
   báo task hoàn thành.

Agent không bao giờ merge `release/qc` vào nhánh feature. Các repo không có
`release/qc` (`bom-bartender-app`, `bom-monitor-*`, `bom-base`, `plm-base`) đi thẳng
vào `main`. `taskpilot doctor` báo khi một nhánh đích không tồn tại trên origin.

Token Bitbucket chỉ nằm trong tiến trình MCP của taskpilot, không bao giờ đi qua
agent. **Không có token:** nhánh vẫn được push bằng SSH key của máy; trong TUI, bấm
`p` để mở trang tạo PR, tiêu đề và mô tả đã được copy sẵn. **Có token**
(`taskpilot auth bitbucket`): PR được tạo tự động.

### Splunk

Agent tìm log qua MCP tool **`splunk_search`**: khi điều tra bug hay sự cố prod
(trước khi lập plan), và khi kiểm tra log QC sau khi deploy.

- **Chỉ đọc.** Chặn các lệnh ghi hoặc có tác dụng phụ (`outputlookup`, `collect`,
  `delete`, `sendemail`, `script`, `rest`, ...), chặn subsearch và các lệnh ghép
  dữ liệu ngoài phạm vi (`append`, `join`, ...).
- **Theo phạm vi project.** `splunk_filter` của project được ghép bắt buộc vào đầu
  mọi câu tìm kiếm: `(bộ lọc) (tìm kiếm của agent) | ...`.
- **Có giới hạn:** khoảng thời gian (`max_lookback`), số event, độ dài mỗi event,
  và thời gian chạy tối đa của một lần tìm.
- **Mỗi câu truy vấn được ghi lại** trong `~/.taskpilot/tasks/<task>/splunk-queries.jsonl`.
- Credential (token hoặc cookie phiên) chỉ nằm trong tiến trình MCP, không đi qua agent.

```bash
taskpilot auth splunk            # đăng nhập trong Chrome profile riêng (hoặc tab Status, phím s)
taskpilot auth splunk -token     # dán API token tạo ở Splunk: Settings → Tokens (khuyên dùng: sống lâu)
taskpilot splunk search 'kubernetes.container_name=payment-worker level=error' -earliest -15m -project payment
```

### Logs QC (Argo CD)

`argocd.url` trỏ tới Argo CD của môi trường QC. App được gán vào project theo tên
repo có trong tên app; nếu tên repo dài hơn khớp thì ưu tiên tên dài hơn (ví dụ
`payment-gateway-global-qc` thuộc `payment_gateway_global`, không thuộc
`payment`). Có thể khai báo rõ bằng `projects.<p>.argocd.apps` hoặc `.projects`
(AppProject của Argo CD). Kiểm tra cách gán bằng `taskpilot argo apps`.

- Đăng nhập: `taskpilot auth argocd` (Chrome profile riêng; lưu cookie `argocd.token`,
  là JWT nên đọc được hạn) hoặc `-token` để dán token.
- Agent có hai tool **chỉ đọc**: `qc_apps` (bản nào đang chạy trên QC) và `qc_logs`
  (log pod). Agent chỉ thấy app thuộc project của task. Agent dùng chúng sau khi
  PR vào `release/qc` được merge, để xác nhận bản mới đã lên QC.
- **Sync chỉ có trong TUI** (phím `S`, luôn hỏi xác nhận trước).

### Logs Production

Mỗi project khai báo trong config một mục `logs_production`:
- `filter`: bộ lọc log prod, ví dụ `kubernetes.namespace_name=payment-prod`;
- `services`: danh sách tên container;
- `queries`: các truy vấn có sẵn. Truy vấn dùng biến `{filter}`, `{service}` và
  các `{tên}` khác; giá trị nhập vào được escape trước khi ghép vào SPL.

Kiến thức về log (namespace, trường log, quy tắc `request_id`, lỗ hổng đã biết,
cách điều tra) nằm trong skill `prod-logs`
(`internal/plugin/files/skills/prod-logs/`). Agent đọc skill này trước khi tìm
log; trong TUI bấm `i` để đọc cùng nội dung đó. Project có `no_logs: true` (PLM)
thì agent không được tìm Splunk. `taskpilot doctor` kiểm tra mọi truy vấn có sẵn.

### Phím tắt trong TUI

| Phím | Tác dụng |
|---|---|
| `j` / `k` | di chuyển |
| `a` | duyệt yêu cầu đang chờ |
| `x` | từ chối kèm nhận xét (agent sửa lại rồi xin duyệt lại) |
| `m` | gửi message để chạy tiếp task `needs_attention`, `failed` hoặc `done` |
| `o` | take over: mở `claude --resume <session>` trong worktree; daemon tạm dừng task này |
| `f` | lọc inbox: chỉ hiện task đang cần bạn |
| `q` | thoát |

Các lệnh CLI tương đương: `taskpilot list | approve TASK | reject TASK -comment C | message TASK -text M`.

## Đã kiểm chứng

- Unit test và integration test (`go test ./...`), trong đó test daemon chạy toàn
  bộ state machine với một `claude` giả, trên một git repo thật.
- Chạy thật với Claude Code 2.1.319 (sonnet, effort low) trên một repo Go nhỏ.
  Ticket đi hết plan, duyệt, code, `go test`, commit, PR, duyệt, `done` trong 4
  lượt, tổng $0.56. Agent viết tóm tắt bằng tiếng Việt và ticket nhận đủ báo cáo
  tiến độ.
- Hook với `claude` thật: lệnh `Write` khi chưa duyệt plan bị chặn với lý do
  `gate "plan" is not approved yet`.

## Lộ trình

1. **Source Hasaki Work**: cài interface `source.Source` (`Fetch`, `Report`).
   Cần biết API và cách xác thực.
2. **Bộ eval**: khoảng 10 ticket đã xong làm bộ chuẩn, chạy lại mỗi khi đổi skill
   hoặc model, mỗi task ít nhất 2 lần.
3. **Cổng deploy**: thêm skill deploy-qc (ArgoCD MCP ở chế độ read-only cộng với
   sync qua cổng `deploy`), và kiểm tra QC bằng Splunk, PLM qc, BOM qc (MCP tự viết).
4. **Reviewer độc lập** (generator–verifier): chỉ thêm nếu eval cho thấy có ích hơn.
