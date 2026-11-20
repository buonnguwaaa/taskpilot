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
   lấy ticket → git worktree → chạy 1 lượt agent -p → đỗ ở cổng
   → chờ quyết định → agent -p --resume <cùng session>
                        │
     1 session / task (Claude Code hoặc Cursor Agent CLI)
     skill  taskpilot:workflow   (thứ tự công việc)
     skill  taskpilot:ponytail   (viết thay đổi nhỏ nhất đủ dùng)
     MCP    taskpilot            (request_approval, complete_task, task_info)
     hook   PreToolUse/preToolUse (ép cổng: Edit/Write trước plan, push trước pr, ...)
     MCP    argocd / ... theo từng repo (graphify đã có sẵn trong MCP taskpilot)
```

Worker mặc định là Claude Code. Khi Claude hết quota (weekly/session limit, 429),
daemon **tự fallback sang Cursor Agent** (cần `agent` trong PATH hoặc `cursor.bin`),
ghi latch `~/.taskpilot/claude-quota.json`, ghim task sang Cursor, và chạy lại lượt đó
trên session Cursor mới (không resume được session Claude).

```yaml
agent: claude          # mặc định
cursor:
  bin: agent           # cài: curl https://cursor.com/install -fsS | bash
  # fallback_on_quota: false   # tắt fallback
```

`agent: cursor` ép mọi task dùng Cursor. Cần `agent login` hoặc `CURSOR_API_KEY`.

Các nguyên tắc chính, theo bài [When to fold subagents into your main agent loop](https://claude.com/resources/articles/building-agents-when-to-fold-subagents-into-your-main-agent-loop):

- **Một vòng agent cho mỗi task**, không chia thành các subagent planner, coder,
  reviewer. Plan, code và verify dùng chung một context, nên không mất thông tin
  qua các lần chuyển giao.
- **Code tối giản theo [ponytail](https://github.com/DietrichGebert/ponytail)** (MIT): skill
  `taskpilot:ponytail` là bộ rule của ponytail, chỉnh cho luồng taskpilot. Agent đọc nó trước lần sửa
  code đầu tiên và dùng phần "lean check" khi tự review trước cổng `pr`. Test chỉ để agent tự kiểm tra
  trên máy: file test mới không được commit (`create_pull_request` từ chối commit có thêm file test như
  `*_test.go`, `*.test.ts`, `*.spec.ts`, `test_*.py`, `__tests__/`, `testdata/`; sửa test cũ bị hỏng
  thì vẫn được). Subagent `implementer` và
  `reviewer` dùng cùng tiêu chí. Chỉ lấy rule, không lấy hook của plugin ponytail (statusline, ghi
  ledger, webhook), để không chồng lên hook gate của taskpilot.
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
| `git push`, `gh pr create` | `pr` |
| merge PR bằng `gh pr merge`, API `pullrequests/<id>/merge`, push thẳng vào `main`/`master`/`develop`/`release/*` | **luôn bị chặn** |
| tool `merge_pull_request` (merge vào `release/qc`, tự resolve conflict) | chỉ merge trong lượt do **tin nhắn / ghi chú duyệt của bạn yêu cầu merge** khởi động (Claude đọc ý định, không dò chữ); chỉ resolve conflict thì mở PR, không merge |
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
| (mọi tab) | `F2` bật/tắt chuột trong TUI: tắt để kéo bôi đen và copy (hoặc giữ Shift khi kéo) |
| **1 Tasks** | Task của agent. `a` duyệt, `A` duyệt kèm ghi chú, `x` từ chối kèm nhận xét, `m` nhắn agent, `u` thu hồi tin nhắn / quyết định vừa gửi, `p` mở PR, `h` tạo task trên Hasaki Work từ task đã làm (tự chọn EPIC DEV / Improve / Issue Production + sprint của dự án, giao cho bạn), `v` mở/thu gọn Hoạt động, `o` tự làm trong session của agent, `n` tạo task mới bằng form, `N` tạo task từ prompt (mô tả bằng lời, Claude điền form), `f`/`F` lọc danh sách (Tất cả → Đang làm → Tạm hoãn → Đã xong → Cần bạn), `/` tìm task, `c` đánh dấu xong, `z` tạm hoãn / làm tiếp, `d` xoá task. Danh sách chia ba nhóm: Đang làm, Tạm hoãn, Đã xong |
| **2 Hasaki Work** | `n` tạo subtask dưới task đang chọn (mỗi dòng một subtask, cùng dự án và sprint, giao cho bạn). Task của bạn trên Hasaki Work, kèm phạm vi (bom/plm/payment) và mô tả. **Enter** (hoặc click lần hai) mở form giao cho agent: chọn project, repo bắt đầu, ghi chú. `s` đổi bộ lọc |
| **3 Logs** | Bấm `e` để đổi môi trường. **QC · Argo CD** (cả bom, payment, plm): danh sách app kèm health và sync, revision, image; Enter xem log pod, `/` lọc, `t` khoảng thời gian, `o` mở trên Argo CD, `S` sync (hỏi xác nhận). **Production · Splunk** (bom, payment; PLM không có log prod): `g` nhóm lỗi, `n` tạo task điều tra sự cố, truy vấn có sẵn (hỏi tham số), `/` tìm theo điều kiện, `a` hỏi bằng lời, `t` thời gian, `o` Splunk Web, `c` copy SPL, `i` kiến thức log của project |
| **4 Status** | Ba mục, đổi bằng `←/→`. **Tổng quan**: kết nối và đăng nhập (`l` Hasaki Work, `t` dán JWT, `b` Bitbucket…), daemon và lịch quét lỗi, duyệt cổng 7 ngày qua, dự án, log của daemon; `d` chạy doctor. **Usage**: token và chi phí theo khoảng thời gian (`[ ]`: hôm nay, hôm qua, 7/30 ngày, tháng này/trước, 12 tuần, tất cả; `c` chia theo giờ/ngày/tuần/tháng), tách theo model, project, nguồn (task hay lệnh phụ: soạn task, worklog, SPL, gom nhóm lỗi, reflect…) và task tốn nhất; lệnh phụ được ghi từ schema v17. **Agent**: graphify và subagent |

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

Agent chạy ngầm cũng hiện trong herdr: mỗi khi daemon bắt đầu một lượt chạy, nó
mở tab **"tp TASK"** (nếu chưa có) chạy `taskpilot watch TASK`. Tab này hiện hoạt
động mới nhất của agent và báo trạng thái cho herdr (`working` khi chạy,
`blocked` khi chờ duyệt / cần bạn / lỗi), nên task có mặt trong danh sách agent
của herdr. Tab chỉ để xem (duyệt, nhắn, làm tay vẫn ở TUI) và tự đóng khi task
xong; đóng tay thì lượt chạy sau mở lại.

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
  `./<repo>` trên nhánh tạm `tp/<task>`, lấy từ `origin/<base>` vừa fetch. Agent chỉ
  mở được các repo **thuộc project của task**.
- Trước commit đầu tiên, agent đặt tên nhánh theo convention, bằng tool `set_branch` hoặc tự `git branch -m`:
  `<type>/<mô-tả-tiếng-anh>--<FUNCTION_CODE>`, ví dụ
  `feat/add-hawaiian-lifestyle-category--PLM_DSN_STY_0013` (mã chức năng lấy từ footer
  `func:` của các commit cùng chỗ; mã ticket `--HSK-…` khi không có mã chức năng). Tool đổi
  tên trong mọi repo đã mở cùng lúc. Agent cũng được tự đổi tên bằng git: `create_pull_request`
  push nhánh mà repo đang đứng, ghi lại tên đó cho task, và chỉ từ chối tên sai convention.
  Đổi tên sau khi đã có PR thì PR mới được mở từ tên mới, PR cũ vẫn mở trên Bitbucket (agent
  sẽ nhắc bạn decline). Mẫu kiểm tra mặc định có thể ghi đè bằng
  `projects.<p>.branch_pattern` (regexp).
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
taskpilot submit -prompt "export MO bị timeout khi >10k dòng"   # Claude soạn ticket, in ra, hỏi y/N
echo "…" | taskpilot submit -prompt - -yes                      # đọc yêu cầu từ stdin, gửi luôn
```

**Tạo task từ prompt** (`N` trong TUI, `submit -prompt` ở CLI): một lượt `claude -p`
ngắn, **không có tool** và không lưu session, đọc yêu cầu cùng danh sách project và repo
(kèm `description` của repo) rồi trả về project, repo bắt đầu, tiêu đề và mô tả. Project
hoặc repo không có trong config sẽ bị bỏ trống để bạn tự chọn. Yêu cầu gốc luôn được
giữ nguyên ở cuối mô tả. Không có gì vào inbox cho tới khi bạn xác nhận. Các flag
`-project`, `-repo`, `-title`, `-body` truyền kèm sẽ ghi đè bản nháp.

### Log production (tab 3 → `e` Production)

- **`g` Nhóm lỗi theo service**: đếm lỗi của mọi service, gộp các thông điệp giống nhau (số, mã, UUID được che) thành
  một nhóm, nhiều nhất trước. Chọn nhóm để xem caller, prefix, request_id, stacktrace; `Enter` xem log của nhóm
  (lọc theo `caller` + cụm từ cố định), `n` tạo **task điều tra** (xem dưới), `f` tạo task sửa ngay (dự án, repo trùng
  tên service, tiêu đề, mô tả có bằng chứng và SPL), `s` gom lại chỉ cho service đó.
- **`n` Điều tra sự cố production**: từ một nhóm lỗi, từ kết quả tìm log đang xem, hoặc từ mục **🔎 Điều tra sự cố…**
  (mô tả triệu chứng khách / vận hành báo, mã đơn / giao dịch, khoảng thời gian). Taskpilot mở form task mới điền sẵn
  ticket điều tra: bằng chứng ban đầu, khoảng thời gian chốt thành **mốc tuyệt đối** (agent chạy sau vẫn tìm đúng chỗ) và
  việc phải giao. Agent dựng diễn biến từ log, lần theo request qua các service, đọc code tại caller, xem thay đổi gần
  đây, kiểm tra dữ liệu trên DB QC, rồi gửi **báo cáo nguyên nhân gốc + ảnh hưởng + đề xuất sửa ở cổng `plan`**; chỉ sửa
  code khi bạn duyệt. Không cần sửa code (lỗi đối tác, dữ liệu, cấu hình) thì agent kết thúc bằng báo cáo.
- **`/` Tìm theo điều kiện**: chọn service, mức (error / warn+error / mọi mức), chuỗi cần tìm, request_id, bỏ log nhiễu;
  SPL được tự sinh (hoặc tự viết vào ô SPL).
- **`a` Hỏi bằng lời**: Claude (không có tool) đọc tài liệu log của dự án và viết SPL + khoảng thời gian; bạn xem, sửa
  rồi mới chạy. SPL luôn qua bộ kiểm tra chỉ-đọc, bộ lọc prod được thêm khi chạy.
- **`t`/`T` thời gian**: theo truy vấn, 15 phút, 1 giờ, 4 giờ, 24 giờ, hôm nay, hôm qua, 7 ngày.
- Truy vấn thống kê (`stats`, `top`…) hiện thành bảng — trước đây chúng luôn trả 0 dòng.

### DB QC (chỉ đọc)

Agent có tool `db_query` để đọc DB QC (MySQL/MariaDB) của dự án khi debug hoặc kiểm tra dữ liệu sau khi deploy QC.
Thông tin đăng nhập không bao giờ tới tay agent: taskpilot đọc nó lúc chạy query, từ file mà repo đã có
(`qc_db.file` + `env` cho file KEY=VALUE, hoặc `yaml` cho file YAML), hoặc từ kho secret (`taskpilot auth set-db PROJECT`).

- Chỉ nhận một câu `SELECT` / `SHOW` / `DESCRIBE` / `EXPLAIN`; session và transaction đều READ ONLY, server tự ngắt
  query quá `timeout` (mặc định 30s), tối đa `max_rows` dòng (mặc định 200).
- Mọi query của agent được ghi vào bảng `db_queries` (target, câu SQL, số dòng, lỗi).

```yaml
projects:
  payment:
    qc_db: {file: ~/repos/bitbucket/payment/payment/build/.base.env, env: DB_DATA_SOURCE}
  bom:
    qc_db:
      file: ~/repos/bitbucket/BOM/bom-api/config/local.yml
      yaml: {host: MySql.MySqlHost, port: MySql.MySqlPort, user: MySql.MySqlUser, password: MySql.MySqlPassword, database: MySql.MySqlDbname}
  plm:
    qc_db: {}          # DSN trong kho secret: taskpilot auth set-db plm
```

```bash
taskpilot qcdb list                                   # DB QC của từng dự án (không in mật khẩu)
taskpilot qcdb query bom "SHOW TABLES"                # tự query thử, cùng giới hạn như agent
taskpilot auth set-db plm                             # dán DSN user:pass@tcp(host:3306)/db (không hiện ký tự)
```

### Vault QC (chỉ đọc)

Agent đọc config/secret của các service **trên QC** bằng `qc_config_list` / `qc_config_get` — chỉ đọc, chỉ trong các đường
dẫn khai báo ở `projects.<p>.vault_paths`, mỗi lần đọc được ghi vào bảng `vault_reads` (đường dẫn, số khoá, lỗi; không lưu
giá trị). Lệnh `vault` CLI vẫn bị chặn với agent; production không nằm trong phạm vi.

```bash
taskpilot auth vault          # mở Chrome profile riêng, đăng nhập SSO (OIDC); -token để dán token
taskpilot vault ls            # các secret engine; `vault ls kv/payment/` các khoá bên dưới
taskpilot vault get PATH      # tự xem một secret
```

```yaml
vault:
  url: https://vault-qc-v2.hasaki.dev
projects:
  payment:
    vault_paths: [kv/payment/]   # ví dụ — đặt theo cấu trúc thật của Vault QC
```

### Graphify: hiểu code xuyên repo

Agent hiểu code của cả project, không chỉ repo đang mở. Taskpilot dựng **graph cho từng
repo theo từng dòng code**: `prod` (`release/prod`, repo nào không có thì dùng nhánh base) và
`qc` (`release/qc`).

- **Nguồn:** lấy nhánh **trên remote** (fetch, rồi `git archive` ra snapshot), không đụng repo hay nhánh
  bạn đang code. graphify phân tích snapshot ở chế độ chỉ code (`--code-only`), chạy local, không dùng LLM.
  Code sinh từ proto (`*.pb.go`) được bỏ qua.
- **Nối repo:** taskpilot tự đọc code gRPC sinh ra trong mỗi repo, tìm chỗ gọi client (kể cả client
  nhúng trong struct riêng) và handler đã đăng ký server, rồi nối hai bên theo tên đầy đủ của method
  `/pkg.Service/Method`. Các liên kết này được chèn vào graph gộp của project dưới dạng cạnh
  `calls_grpc`, nên `affected`, `path` và `explain` của graphify đi xuyên repo. Ví dụ
  `bom-worker genWOViaBomReport → /generate.GenerateService/GenerateWorkOrderV2 → bom-report`.
- **Agent dùng:** tool MCP `graphify` (dùng được cho cả Claude và Cursor). Các action: `links`,
  `affected`, `path`, `explain`, `query`, và `read` để đọc code của nhánh `release/qc` hay
  `release/prod`, vốn không có trong worktree của task. Dòng mặc định là `qc` cho task điều tra QC,
  `prod` cho các task còn lại.
- **Graph của task (`line: task`):** gồm các repo task đã mở, lấy từ worktree (nhánh của task, tạo
  từ `main` mới nhất, có cả thay đổi chưa commit), cộng các repo còn lại lấy từ dòng prod; liên kết
  gRPC được nối như các dòng khác. Lần dựng đầu dùng lại cache của dòng prod nên chỉ phân tích những
  file khác. Mỗi khi agent sửa code, graph tự dựng lại trước khi trả lời, và chỉ phân tích lại file
  đã đổi. Đây là dòng mặc định khi task đã mở repo, và bị xoá cùng workspace.
- **Khi task mới bắt đầu:** worktree luôn tạo từ `origin/main` vừa fetch (fetch lỗi thì thử lại 3 lần,
  vẫn lỗi thì không mở repo, không bao giờ dùng `main` cũ). Daemon cũng fetch `release/prod` và
  `release/qc` mới nhất rồi graphify lại các repo có commit mới, trước lượt chạy đầu của agent.
- **Cập nhật:** daemon kiểm tra theo `graphify.refresh` (mặc định 30 phút) và chỉ dựng lại repo nào
  có commit mới. Có thể dựng tay bằng `taskpilot graphify build [PROJECT] [-line qc] [-force]`.
- Xem trạng thái bằng `taskpilot graphify status` hoặc ở tab 4 Status, xem liên kết bằng
  `taskpilot graphify links bom GenerateWorkOrder`, và thử truy vấn như agent bằng
  `taskpilot graphify ask bom prod affected <node>`.

```yaml
graphify:
  refresh: 30m
  # lines: {prod: "release/prod,$base", qc: release/qc}   # mặc định
projects:
  plm:
    repos:
      some-repo: {lines: {qc: qc}}   # repo có nhánh QC tên khác; {qc: "-"}: không có dòng qc
```

Cài graphify: `uv tool install graphifyy` (hoặc pip vào một venv rồi link `graphify` vào PATH).

### File đính kèm và prototype claude.ai trong task Hasaki Work

Khi giao task từ tab Hasaki Work (Enter), taskpilot tải về `refs/hw-<code>/` cạnh task và liệt kê trong đề bài:

- **File đính kèm mô tả** (`note_files`: ảnh, pdf, xlsx…). Các file này công khai trên `hr-media.hasaki.vn`, không cần đăng nhập.
- **Prototype claude.ai** (các link `https://claude.ai/artifact/...` trong mô tả và trong ghi chú của bạn): mã nguồn
  của artifact (với Design: `project/*.dc.html`, `project/canvas.json`) và một ảnh chụp. Agent cũng gọi được
  `read_claude_artifact` cho link claude.ai gặp ở chỗ khác.

Link claude.ai chỉ mở được khi đã đăng nhập. Agent chạy headless không có công cụ Artifact, nên taskpilot mở link bằng
Chrome profile riêng, kèm cookie `sessionKey` của bạn. Chrome thường không cho điều khiển từ xa và mã hóa cookie, nên
cookie phải được dán một lần:

```bash
taskpilot auth claude        # dán sessionKey (F12 → Application → Cookies → https://claude.ai); không hiện ra màn hình
taskpilot claude whoami      # kiểm tra phiên
taskpilot claude artifact 'https://claude.ai/artifact/...?sk=...' -o /tmp/x   # tự tải một artifact
```

Nếu Chrome riêng chưa mở thì taskpilot tự mở, tải xong rồi đóng; nếu đang mở thì chỉ mở rồi đóng một tab.

### Kafka QC qua Kafka UI (Kafbat, chỉ đọc)

Agent xem topic, message và consumer group của Kafka **QC** bằng `qc_kafka_topics`, `qc_kafka_messages`,
`qc_kafka_consumer_groups` — chỉ đọc (không produce, không reset offset), chỉ trong các topic khai báo ở
`projects.<p>.kafka_topics` (tên chính xác hoặc glob). Đăng nhập là phiên SSO (cookie `SESSION`) của Kafka UI, lưu
trong secret store; phiên hết hạn thì đăng nhập lại (tab Status, phím `K`).

```bash
taskpilot auth kafka                          # mở Chrome profile riêng, đăng nhập Google SSO; -token để dán Cookie
taskpilot kafka whoami                        # kiểm tra phiên, liệt kê cluster
taskpilot kafka topics payment                # topic có chứa "payment"
taskpilot kafka messages TOPIC -filter SO123 -limit 10
taskpilot kafka messages TOPIC -since 2h      # đọc xuôi từ 2 giờ trước (-from-offset O -partition P)
taskpilot kafka groups TOPIC                  # consumer group và lag; `kafka group ID` chi tiết từng partition
```

```yaml
kafka:
  url: https://kafka-ui-qc-v2.hasaki.dev
  # cluster: qc                 # bắt buộc khi Kafka UI có nhiều cluster
  # max_messages: 50            # trần số message mỗi lần agent đọc
projects:
  payment:
    kafka_topics: [payment-*, "*.payment.*"]   # ví dụ — đặt theo tên topic thật
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

### Pull request trên Bitbucket: release/qc và main

Luồng của team, khai báo bằng `pr_flow` trong config (có thể ghi đè cho từng repo):

```
main ──checkout──► feat/<mô-tả>--<FUNC_CODE>   (tp/<task> cho tới khi agent đổi tên)
                     ├─ PR ①  → release/qc   (sau khi duyệt cổng pr)  → QC test
                     └─ PR ②  → main         (cũng sau cổng pr, mở cùng lúc, từ CHÍNH nhánh feature)
main → release/prod: bước release do người làm, không phải việc của agent
```

1. Bạn duyệt `pr`. Agent gọi `create_pull_request` cho từng repo đã sửa: push nhánh,
   mở PR vào `release/qc` rồi vào `main` (hoặc `master`, tùy `base`). Sau đó agent xin
   duyệt **`qc`**, kèm checklist test QC. PR vào `main` chờ team merge khi QC đạt.
2. QC báo lỗi: bạn từ chối `qc` và mô tả lỗi. Agent sửa ngay trên nhánh đó; gọi lại
   tool sẽ push commit mới, cập nhật cả hai PR. Agent xin duyệt `qc` lại.
3. QC đạt: bạn duyệt `qc`. Agent báo task hoàn thành.

**Cho agent merge vào `release/qc`:** nhắn agent (phím `m`, hoặc ghi chú khi duyệt/trả lời)
một câu **yêu cầu merge**, ví dụ "merge PR frontend vào qc đi", "resolve conflict rồi merge luôn".
Taskpilot không dò chữ: trước khi merge, nó hỏi Claude (`claude.classify_model`, mặc định haiku, một lượt ngắn) xem tin nhắn có thật
sự bảo merge không và cho repo nào; taskpilot tự so repo. "resolve PR frontend merge vào QC" hay "fix conflict" chỉ là resolve:
agent resolve rồi mở PR (`resolve_only`), **không merge**, và chờ bạn bảo merge. Câu không rõ được
tính là không. Chỉ lượt chạy do chính tin nhắn đó khởi động mới được merge; lượt sau không còn quyền. Nếu PR conflict,
tool chỉ cho agent tạo nhánh `<nhánh-task>--into-release-qc` từ `release/qc`, gộp nhánh task
vào, resolve, rồi gọi lại: tool push nhánh đó, mở PR mới, merge, và decline PR cũ. Nhánh task
(và PR vào `main`) không bị lẫn commit của `release/qc`. Đổi nhánh được phép bằng
`agent_merge_targets` trong config (mặc định `[release/qc]`).

Muốn một repo chỉ mở PR vào `main` sau khi QC đạt (luồng cũ), đặt cho repo đó
`pr_flow: [{target: release/qc, gate: pr}, {target: $base, gate: qc}]`.

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

### Jenkins (chỉ đọc)

```yaml
jenkins:
  url: https://jenkins.hasaki.vn
  # depth: 3            # số tầng folder / multibranch được liệt kê (folder/repo/branch)
  # max_log_lines: 400  # số dòng console tối đa mỗi lần đọc
projects:
  bom:
    jenkins:
      jobs: ["BOM/"]    # (tuỳ chọn) đoạn tên job thuộc project; mặc định khớp theo tên repo
```

- Đăng nhập: `taskpilot auth jenkins`. Tạo API token ở `<jenkins>/me/configure` → API Token, rồi nhập user
  và token. Taskpilot kiểm tra token trước khi lưu vào kho secret; agent không thấy token.
- Agent có ba tool **chỉ đọc**: `jenkins_jobs` (job của project, trạng thái build cuối), `jenkins_builds`
  (các build gần nhất: kết quả, commit/branch, nguyên nhân, tham số, commit được build) và `jenkins_log`
  (console log: phần cuối, hoặc các dòng khớp `filter` kèm ngữ cảnh). Không có tool nào chạy, dừng hay sửa
  build. Job/build có thể truyền bằng tên đầy đủ (`BOM/bom-api/release%2Fqc`) hoặc link web.
- Agent dùng các tool này sau khi PR vào `release/qc` được merge: build có pass không, nếu fail thì lỗi ở đâu,
  trước khi kiểm tra bản deploy bằng `qc_apps`.

### Logs: QC và Production cùng một giao diện

Tab 3 có hai môi trường (`e` để đổi) với **cùng bố cục và cùng phím**; chỉ nguồn log khác nhau:

- Khung trái: môi trường · dự án (`←/→`) · thời gian (`t`/`T`: theo mặc định, 15 phút, 1 giờ, 4 giờ, 24 giờ, hôm nay,
  hôm qua, 7 ngày) · các thao tác chung **⚠ Nhóm lỗi**, **🔎 Điều tra sự cố**, **✎ Tìm theo điều kiện** · phần riêng của
  môi trường: Production có ✨ Hỏi bằng lời + truy vấn có sẵn, QC có danh sách app (health, sync; `Enter` xem log pod,
  `S` sync).
- Phím: `Enter` chạy · `g` nhóm lỗi · `n` điều tra (từ nhóm lỗi, log đang xem, hoặc mô tả triệu chứng) · `/` tìm theo
  điều kiện (service, mức, chuỗi, request_id) · `r` chạy lại · `c` copy · `o` mở web (Splunk Web / Argo CD).
- Danh sách nhóm lỗi giống nhau: `↑↓` chọn · `Enter` xem log · `n` điều tra · `f` sửa ngay · `s` chỉ service đó · `esc`.
- Dòng log cùng định dạng: thời gian · mức · service · thông điệp · request_id.

Log QC **không có trên Splunk**: taskpilot đọc log pod của các app qua Argo CD rồi lọc, gom nhóm tại chỗ. Task điều tra
QC hướng agent dùng `qc_apps` (bản đang deploy) và `qc_logs`, đọc code và DB QC, báo cáo ở cổng plan.

### Tự quét lỗi production theo lịch

Khai báo `logs_production.scan` cho dự án; daemon quét ở các khung giờ đó (gom nhóm lỗi như phím `g`, trên khoảng thời
gian từ lần quét thành công trước, tối đa `max_window` = 24h), lưu các nhóm thành **đề xuất** và **không tự tạo task**:

```yaml
projects:
  payment:
    logs_production:
      filter: kubernetes.namespace_name=payment-prod
      scan:
        times: ["09:00", "13:30", "17:00"]   # giờ máy
        min_count: 3                         # nhóm ít hơn thì bỏ qua
        ignore: ["context canceled"]         # lỗi nhiễu đã biết
        services: []                         # trống = mọi service
        max_window: 24h                      # mỗi lần quét đọc tối đa từng này; danh sách cũng chỉ hiện lỗi còn thấy trong khoảng này
```

- Tab 3 Logs → Production → **🔔 Đề xuất sửa lỗi (N)** (số đề xuất cũng hiện trên tên tab): `n` tạo task điều tra, `f`
  tạo task sửa, `Enter` xem log của nhóm, `x` bỏ qua hẳn, `z` ẩn 7 ngày (hiện lại nếu lỗi còn; cả hai hỏi xác nhận), `v` xem các đề xuất đã ẩn (`u` bỏ ẩn, `U` bỏ ẩn tất cả), `r` quét ngay (ngoài lịch; hoặc `taskpilot scan run PROJECT`).
- Danh sách chỉ hiện nhóm lỗi còn xuất hiện trong `max_window` (mặc định 24h) tính tới lần quét thành công gần nhất; nhóm cũ hơn tự ẩn và hiện lại nếu lần quét sau thấy lại.
- Lỗi lặp lại ở các lần quét sau cộng dồn vào cùng một đề xuất; đề xuất đã có task hoặc đã bỏ qua không hiện lại. Đề xuất
  chỉ được đánh dấu "đã có task" khi bạn thật sự gửi form.
- Quét lỗi (Splunk không tới được…) thì thử lại sau 15 phút; khoảng thời gian luôn tính từ lần quét **thành công** gần
  nhất nên không bỏ sót.
- CLI: `taskpilot scan run PROJECT` (quét ngay), `taskpilot scan list [PROJECT]`.

### Quy trình debug và reflect

Những bước tra cứu bạn hay lặp lại khi debug ("check đơn X": dòng trong DB QC, log production, log QC…) được đóng gói
thành **quy trình**. Mỗi quy trình là một danh sách tra cứu chỉ-đọc có tham số, cộng với hướng dẫn cách đọc kết quả.

- **Chạy**: tab 3 Logs, mục **Quy trình debug** (Production), chọn quy trình rồi Enter, điền tham số. Taskpilot tự chạy
  các bước, **không tốn Claude**. Xong bấm `n` để giao kết quả cho agent điều tra tiếp, hoặc `c` để copy.
- **Agent dùng**: mỗi quy trình thành một skill `recipe-<tên>` trong plugin, và agent chạy nó bằng tool MCP
  `run_recipe`. Agent chỉ thấy quy trình của project mình. Thay đổi có hiệu lực từ lượt chạy tới của agent.
- **Reflect** (`R` trong mục, hoặc `taskpilot reflect -project P`): một lượt Claude không có tool. Claude đọc các task
  **có tra cứu** của project kể từ lần reflect trước (lần đầu: 30 ngày), gồm tin bạn nhắn, các truy vấn agent đã chạy
  (đánh dấu lệnh lỗi) và kết luận, rồi đề xuất 0–3 quy trình. Chi phí khoảng $0,5–1 mỗi project.
- **Duyệt đề xuất** (dòng ◆): Enter chạy thử, `a` chấp nhận, `e` sửa YAML trong `$EDITOR`, `x` bỏ (không đề xuất lại).
  Với quy trình đã lưu: `e` sửa, `d` xoá. **Chưa chấp nhận thì không có gì được dùng.**
- **An toàn**: các bước chỉ thuộc các loại `sql` (DB QC), `splunk`, `qc_logs`, `vault`, `kafka`. Mỗi bước đi qua đúng các
  kiểm tra như tool của agent (SQL chỉ đọc, filter prod, `vault_paths`, `kafka_topics`, app QC của project). Giá trị tham
  số không được chứa nháy, `\`, `;`, `|`, ngoặc hay xuống dòng.
- Quy trình nằm trong `~/.taskpilot/recipes/<tên>.yaml`: sửa tay được, đưa vào git được. Ví dụ:

```yaml
name: tra-don-payment
project: payment
description: Tra một đơn/transaction payment: trạng thái trong DB QC và log production.
params:
  - {name: code, desc: order code hoặc HSK_T…, example: 261116VN91SQ}
steps:
  - title: Transaction trong DB QC
    sql: {query: "SELECT id, status, created_at FROM transactions WHERE order_code = '{code}' LIMIT 20"}
  - title: Log production
    splunk: {query: '"{code}"', earliest: -1d}
  - qc_logs: {app: payment-qc-payment, filter: "{code}"}
guide: |
  status = … mà không có callback thì xem worker …
```

- CLI: `taskpilot recipe list|show|run NAME k=v|run -file F k=v|check F|add F|rm NAME`,
  `taskpilot reflect [-project P] [-days N] [-dry]` (`-dry` in ra những gì Claude sẽ đọc, không gọi Claude),
  `taskpilot reflect list|show ID|accept ID|reject ID`.

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

### Xem code của agent trong Cursor / VS Code

Nhánh của task đang được worktree của task giữ, nên git không cho checkout nó trong repo của bạn. Không cần checkout:
mỗi repo agent mở **đã là một thư mục trên đúng nhánh đó** (`~/.taskpilot/worktrees/<task>/<repo>`). Ở tab 1 bấm **`w`**
để mở workspace của task trong IDE (một repo thì mở thẳng repo đó); Source Control của Cursor hiện từng repo và thay đổi
chưa commit, còn commit của agent so với base thì xem bằng `git diff origin/<base>...HEAD` trong terminal của IDE.
Lúc agent đang chạy chỉ nên xem. Task xong thì worktree **vẫn được giữ** (để quay lại làm tiếp, agent còn đọc được code)
nhưng chuyển sang **detached** ở đúng commit cuối, nên nhánh được nhả: bạn `git checkout <nhánh-task>` trong repo của mình
như bình thường. Khi bạn nhắn lại task, trước lượt chạy taskpilot gắn worktree về nhánh (và tạo lại worktree của repo nào
từng bị gỡ ở bản cũ). Nếu lúc đó bạn vẫn đang đứng trên nhánh ấy trong repo của mình, task chuyển sang `needs_attention`
và báo repo nào: chuyển repo đó sang nhánh khác rồi nhắn lại, tin nhắn trước không bị mất. Commit bạn thêm trên nhánh lúc
checkout thì worktree theo luôn khi gắn lại.

Muốn xem **trong repo chính của bạn** (chạy app, mở bằng project Cursor quen thuộc): bấm **`W`**. Mỗi repo của task được
chuyển sang **commit mới nhất của nhánh task ở dạng detached** (`git switch --detach`) — git không cho checkout chính nhánh
đó khi worktree của agent đang giữ, còn detached thì được và không ảnh hưởng agent. Repo phải sạch (có thay đổi dở thì
taskpilot không đụng vào). Bấm `W` lần nữa: nếu agent đã commit thêm thì cập nhật lên commit mới, nếu đã mới nhất thì quay
về nhánh bạn đang đứng trước đó. Taskpilot lấy đúng nhánh worktree đang đứng (agent có thể đã đổi tên nhánh).
IDE lấy từ `editor:` trong config, mặc định `cursor` rồi `code`.

### Agent hỏi, bạn chọn nhanh

Khi cần quyết định, agent hỏi bằng câu hỏi có lựa chọn (tool `ask_operator`, hoặc `AskUserQuestion` của Claude Code —
taskpilot chuyển nó cho bạn vì phiên chạy headless). Task hiện **❓ Agent hỏi bạn** với các lựa chọn đánh số:

- **Chọn ngay trên khung chi tiết** (câu hỏi riêng lẫn yêu cầu duyệt có kèm câu hỏi): **`1`–`9`** chọn đáp án cho câu
  đang trỏ (▸) rồi tự sang câu kế; câu chọn nhiều thì bấm số để bật/tắt; **`0`** tự gõ câu trả lời; **←/→** đổi câu;
  **Enter** gửi (với yêu cầu duyệt: duyệt + gửi câu trả lời), **`x`** từ chối + gửi. Một câu hỏi duy nhất thì bấm số là
  gửi luôn. Câu hỏi được hiện ngay dưới phần tóm tắt, trước chi tiết dài.
- **`a`**: form trả lời từng câu — ←/→ chọn, **Space** đánh dấu khi câu hỏi cho chọn nhiều, "Khác…" để tự gõ, thêm ghi
  chú và đính kèm tệp.
- **`m`**: trả lời tự do. Với yêu cầu duyệt (plan, pr…) `m` là góp ý mà **không duyệt**: agent sửa rồi xin duyệt lại.
- Yêu cầu duyệt có kèm câu hỏi (vd plan phụ thuộc lựa chọn): `a` vừa duyệt/từ chối vừa trả lời trong một form.

### Thu hồi tin nhắn hoặc quyết định (gõ nhầm)

Phím `u` trên task thu hồi thứ cuối cùng bạn gửi cho agent, không giới hạn thời gian:

- Tin nhắn agent chưa nhận: xoá luôn, form mở lại với nội dung để sửa.
- Đã thành một lượt chạy (tin nhắn, duyệt, từ chối, trả lời câu hỏi): taskpilot hỏi xác nhận, dừng lượt đó nếu agent
  đang chạy, đánh dấu lượt là đã thu hồi và **tua session của agent về trước lượt đó** (`claude --resume-session-at`),
  nên agent không còn thấy nội dung cũ. Tin nhắn bị xoá, hoặc yêu cầu duyệt lại chờ bạn; yêu cầu / hoàn thành agent tạo
  trong lượt đó bị bỏ. Form mở lại với nội dung cũ để bạn sửa và gửi lại.
- Bấm `u` nhiều lần để lùi tiếp từng lượt do bạn khởi động.
- Không hoàn tác được: file và commit agent đã làm trong worktree ở lượt đó, PR đã mở/merge, việc đã làm bên ngoài
  (Hasaki Work, ...). Task chạy bằng Cursor không tua được session.

### Đính kèm tệp khi nhắn agent

Trong mọi ô gõ cho agent (tạo task `n`/`N`, nhắn `m`, duyệt/từ chối kèm ghi chú `A`/`x`, giao task Hasaki Work):

- **Ctrl+O** mở bộ chọn tệp (mặc định ở thư mục Downloads của Windows), Enter để chèn tham chiếu; hoặc
- gõ **`@đường/dẫn`**, `@"đường dẫn có dấu cách"`, hay kéo-thả tệp từ Explorer sau dấu `@` (`@C:\Users\…` được đổi sang `/mnt/c/…`).

Khi gửi, tệp được chép vào `~/.taskpilot/refs/<task>/` và tin nhắn có thêm mục "Tệp đính kèm" để agent đọc bằng Read
(text, PDF, ảnh). Tệp không tồn tại hoặc là thư mục bị báo lỗi ngay trong form, chưa gửi gì. `@team` hay email không
bị coi là tệp. CLI cũng nhận `@tệp` trong `submit -body`, `message -text`, `approve/reject -comment`. Xoá task thì xoá
luôn các bản chép.

### Phím tắt trong TUI

| Phím | Tác dụng |
|---|---|
| `j` / `k` | di chuyển |
| `a` | duyệt yêu cầu đang chờ |
| `x` | từ chối kèm nhận xét (agent sửa lại rồi xin duyệt lại) |
| `m` | gửi message để chạy tiếp task `needs_attention`, `failed` hoặc `done` |
| `o` | take over: mở `claude --resume <session>` trong worktree; daemon tạm dừng task này |
| `n` | tạo task mới bằng form |
| `h` | task lấy từ Hasaki Work: **tạo một subtask** dưới task đó (cùng dự án, sprint), giao cho bạn, ghi **kết quả công việc** (tóm tắt hoàn thành + link PR) và **giờ thực tế**, rồi đánh **Hoàn thành / Chờ duyệt** cho subtask — task cha giữ nguyên. **Tên và kết quả do Claude soạn sẵn** (một lượt không tool, đọc ticket, tóm tắt hoàn thành, PR, lịch sử duyệt và văn phong tên task của team; link PR luôn được giữ) để bạn sửa trước khi gửi; `H` tạo nhiều subtask trống. Task tạo trong taskpilot: tạo task trên Hasaki Work từ task đang chọn: chọn dự án, nhập tên; EPIC (DEV / Improve / Issue Production, đoán theo tiêu đề) và sprint hiện tại được điền tự động, task giao cho bạn, rồi ghi kết quả công việc (tóm tắt hoàn thành + link PR) và đánh Hoàn thành / Chờ duyệt. Ghi thật lên Hasaki Work. Đẩy lại task đã đẩy sẽ tạo thêm task mới (form có cảnh báo) |
| `v` | mở/thu gọn phần Hoạt động |
| `c` | đánh dấu task xong bằng tay (việc đã làm ngoài agent, hoặc không cần nữa): kèm ghi chú làm tóm tắt hoàn thành; yêu cầu duyệt đang treo được đóng, agent không chạy tiếp |
| `d` | xoá task (hỏi lại trước): xoá lịch sử, lượt chạy, workspace và ticket trong inbox; giữ nhánh git, PR trên Bitbucket và task trên Hasaki Work. Không xoá được task đang chạy |
| `/` | tìm task theo id, tên, project, nhánh, ticket (gõ không dấu được, nhiều từ thì phải khớp hết); Enter giữ bộ lọc, Esc xoá |
| `N` | tạo task từ prompt: gõ yêu cầu bằng lời, Claude chọn project/repo, viết tiêu đề + mô tả + acceptance criteria, rồi mở form điền sẵn để bạn sửa và `Ctrl+S` gửi |
| `f` / `F` | đổi bộ lọc (tới / lùi): Tất cả → Đang làm → Tạm hoãn → Đã xong → Cần bạn |
| `z` | tạm hoãn task đang dừng (chuyển sang nhóm Tạm hoãn, không tính vào "cần bạn"); bấm lại để làm tiếp. Nhắn/duyệt cho agent chạy lại hoặc đánh dấu xong thì tự bỏ tạm hoãn |
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
