# Thiết kế database

Schema đầy đủ nằm ở `internal/db/migrations/0001_init.sql`. File này giải thích
lý do của từng quyết định, đặc biệt là phần secret.

## Tổng quan

```
tasks ─┬─< task_transitions        lịch sử trạng thái
       ├─< runs ─┬─< tool_calls    mỗi lần gọi claude -p, mỗi tool call
       │         └─< run_tokens    vé dùng một lần cho mcp-exec
       ├─< approvals               yêu cầu duyệt cùng quyết định
       ├─< operator_messages
       └─< outbox                  báo cáo về Hasaki Work, có retry
eval_cases ─< eval_results
encryption_keys ─< secrets ─< secret_bindings
                          └─< secret_access_log
```

| Bảng | Trả lời câu hỏi |
|---|---|
| `tasks` | Task đang ở đâu, ai đang điều khiển, tốn bao nhiêu |
| `task_transitions` | Task kẹt ở bước nào, chờ tôi bao lâu |
| `runs` | Chi phí, token, thời gian cho mỗi lượt; model và phiên bản skill đã dùng |
| `approvals` | Tỷ lệ duyệt theo cổng (`v_gate_stats`), inbox (`v_inbox`) |
| `tool_calls` | Agent hay bị chặn ở đâu (`v_denials`), có nạp đúng skill không |
| `operator_messages` | Bạn đã can thiệp những gì |
| `outbox` | Báo cáo nào chưa gửi được về ticket |
| `eval_*` | Đổi skill hoặc model thì chất lượng tăng hay giảm |
| `secrets*`, `run_tokens` | Credential cho các tích hợp (xem bên dưới) |

### Ràng buộc do DB đảm bảo
- Mỗi task chỉ có **tối đa một yêu cầu duyệt đang chờ** (partial unique index
  `approvals_one_pending`). Trước đây ràng buộc này nằm trong code của MCP server.
- `decision` và `decided_at` phải cùng có hoặc cùng không.
- `state`, `actor`, `trigger`, `consumer_kind` chỉ nhận giá trị trong danh sách cho phép.
- Bảng `STRICT`: ghi sai kiểu dữ liệu thì lỗi ngay, không bị tự ép kiểu.

Tất cả các ràng buộc trên đã được thử với SQLite 3.45.

### Những gì không lưu vào DB
- **Stream-json đầy đủ.** Vẫn là file `events.jsonl`; DB chỉ lưu đường dẫn và
  bảng `tool_calls` đã tách sẵn.
- **Code và diff.** Đã có trong git; DB chỉ lưu `base_sha` và `head_sha`.
- **Input thô của tool.** `input_summary` được cắt ngắn và che bớt, vì lệnh Bash
  có thể chứa token.

## Secret

### Mô hình mối đe dọa

Agent (`claude`) chạy **cùng user hệ điều hành** với harness. Tool `Read` và các
lệnh như `rg` trong allowlist đọc được mọi file mà user đó đọc được, kể cả file
DB. Một prompt injection trong ticket, trong code hay trong log Splunk có thể
khiến agent đọc DB rồi đưa nội dung ra ngoài (vào PR, comment hay log).

Vì vậy thiết kế tuân theo bốn nguyên tắc:

1. **Plaintext chỉ khi operator chủ động chọn** (`kind = 'plain'`). Với các
   credential nhạy cảm hơn (prod), dùng `ciphertext` (AES-256-GCM) hoặc tham chiếu
   tới Vault.
2. **Khóa không nằm trong DB và không nằm trong tầm với của agent.**
3. **Secret không bao giờ đi qua tiến trình `claude`:** không qua biến môi trường,
   tham số dòng lệnh, `--mcp-config`, prompt hay log.
4. **Mọi lần truy cập secret đều được ghi lại** trong `secret_access_log`, và bản
   ghi không chứa giá trị.

### Ba loại secret

| `kind` | Lưu gì | Khi nào dùng |
|---|---|---|
| `plain` | `value`, không mã hóa | **Đang dùng** cho token và cookie của Hasaki Work, theo quyết định của operator: DB chỉ chạy local, file có quyền `0600`, và có rule chặn agent truy cập file của harness (xem bên dưới). |
| `vault_ref` | `vault_mount`, `vault_path`, `vault_field` | **Ưu tiên** cho mọi thứ đã có trong Vault. DB chỉ biết đường dẫn; daemon đọc giá trị lúc chạy bằng token Vault của chính nó. |
| `encrypted` | `ciphertext`, `nonce` (12 byte), `key_id` | Credential chưa có trong Vault, hoặc chính token để đăng nhập Vault. |

Mã hóa: AES-256-GCM với nonce ngẫu nhiên cho mỗi lần mã hóa, và **AAD = `name`
của secret**. Nhờ AAD, nếu ai đó tráo ciphertext giữa hai dòng trong DB thì việc
giải mã sẽ thất bại.

### Khóa chính (master key)

Theo thứ tự ưu tiên:
1. **Keyring của hệ điều hành** (Secret Service trên Linux). Trên WSL thường không có.
2. **File khóa** `~/.config/taskpilot/master.key`, quyền `0600`.
3. **Biến môi trường** `TASKPILOT_MASTER_KEY`, chỉ đặt cho tiến trình daemon.

Bất kể dùng cách nào, daemon phải **xóa biến này khỏi môi trường trước khi khởi
chạy `claude`**. Runner hiện đang truyền nguyên `os.Environ()` xuống `claude`, nên
phải sửa chỗ này.

Cột `encryption_keys.fingerprint` giúp phát hiện khi dùng nhầm khóa. Xoay khóa
gồm các bước: thêm khóa mới, đánh dấu khóa cũ là `retired_at`, mã hóa lại các secret
bằng khóa mới (`rewrap`, có ghi log), rồi xóa khóa cũ.

### Đường đi của secret tới MCP server

```
claude ──spawns──> taskpilot mcp-exec --server argocd --token <vé dùng 1 lần>
                         │ unix socket (0600)
                         ▼
                   taskpilot daemon (giữ master key)
                   1. kiểm tra vé: hash có trong run_tokens, chưa dùng, chưa hết hạn,
                      đúng consumer → đánh dấu consumed_at
                   2. lấy secret_bindings của (mcp_server, argocd, repo)
                   3. giải mã hoặc đọc Vault → trả env qua socket
                   4. ghi secret_access_log
                         │
                         ▼
                   mcp-exec exec() server thật với env đó
```

- Trong `--mcp-config` chỉ có vé, không có secret.
- Vé bị tiêu ngay khi MCP server khởi động, tức là trước khi agent kịp làm gì.
  Nếu agent nhìn thấy vé (qua `ps` hay `/proc`) thì dùng lại cũng không được.
- Không có `consumer_kind` nào ứng với `claude`; ràng buộc CHECK trong schema
  ngăn việc gắn secret vào tiến trình agent.

### Che secret trong log

Daemon giữ các giá trị đã giải mã trong bộ nhớ trong suốt một lượt chạy. Trước
khi ghi `events.jsonl`, `tool_calls.input_summary` hay `last_result`, nó thay mọi
chuỗi trùng với giá trị đó (và các dạng base64 hay URL-encode của nó) bằng
`[REDACTED:<name>]`. Cách này có ích phòng khi một MCP server vô tình in token ra
kết quả.

### Chặn agent truy cập file của harness (đã làm)

`config.DefaultRules` có hai rule `deny`. Rule thứ nhất áp cho mọi tool, so khớp
`file_path`, `path` và `notebook_path` (cộng `pattern` của Glob). Rule thứ hai áp
cho lệnh Bash. Cả hai chặn: `taskpilot.db`, `~/.config/taskpilot`, profile Chrome
riêng, `~/.taskpilot/{tasks,plugin,inbox}`, chính thư mục `~/.taskpilot`, và các
đường dẫn tương đối kiểu `../../taskpilot.db` tính từ worktree. Các worktree của
task (`~/.taskpilot/worktrees/…`) vẫn được phép. Các trường hợp này nằm trong
`internal/gate/policy_test.go`.

### Giới hạn còn lại (cần biết rõ)

Khi agent và harness **cùng một user**, các biện pháp trên chỉ là phòng thủ nhiều
lớp, không phải ranh giới bảo mật thật sự. Một lệnh Bash nằm trong allowlist mà
có cách đọc file tùy ý (ví dụ một script test do agent tự viết rồi chạy qua
`go test`) vẫn có thể đọc được file khóa.

**Ranh giới thật sự** là chạy agent dưới **một user hệ điều hành khác** (hoặc
trong container hay devcontainer):
- user `taskpilot`: sở hữu DB, khóa và socket, mọi thứ ở quyền `0600`;
- user `taskpilot-agent`: sở hữu các worktree, chạy `claude`;
- `mcp-exec` kết nối tới socket qua một nhóm quyền riêng, hoặc daemon tự khởi
  chạy MCP server rồi đưa cho agent qua stdio.

Khuyến nghị: dùng cách cùng user khi chạy thử với repo nội bộ ít rủi ro, nhưng
**tách user trước khi đưa credential của prod** (ArgoCD prod, Splunk, Vault) vào.

## Cấu hình kết nối

```
PRAGMA journal_mode = WAL;      -- daemon, TUI, MCP, hook đọc ghi song song
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA synchronous = NORMAL;    -- an toàn với WAL, nhanh hơn FULL
```

- File DB: `~/.taskpilot/taskpilot.db`, quyền `0600`.
- Chuyển trạng thái bằng `UPDATE tasks SET state=? ... WHERE id=? AND state=?`,
  rồi kiểm tra số dòng bị ảnh hưởng. Nhờ vậy hai tiến trình không đè lên nhau.
- Hook chỉ đọc DB.
- Thư viện: `modernc.org/sqlite` (Go thuần, không CGO).
- Migration: nhúng `internal/db/migrations/*.sql` vào binary, áp dụng các file
  có số lớn hơn `PRAGMA user_version`, mỗi file trong một transaction.

## Tình trạng hiện tại (schema v2)

**Toàn bộ trạng thái task đã nằm trong SQLite** (`internal/db/migrations/0002_task_state.sql`):

| Bảng | Bên ghi |
|---|---|
| `tasks`, `task_transitions`, `runs`, `tool_calls`, `outbox` | daemon |
| `approvals` (yêu cầu) và `completions`, `baselines`, `pull_requests`, `splunk_queries` | MCP server của task (agent) |
| `approvals` (quyết định), `operator_messages` | TUI / CLI |
| `takeovers` | `taskpilot takeover` (ghi kèm pid; dòng của tiến trình đã chết bị bỏ qua) |
| `secrets`, `secret_access_log` | `taskpilot auth …` |

- Vẫn để dạng file (DB chỉ lưu đường dẫn): `tasks/<id>/events.jsonl`, `tasks/<id>/runs/*.stderr.log`,
  transcript của Claude Code và các worktree.
- Kết nối dùng WAL, `busy_timeout` và `_txlock=immediate`: mọi transaction ghi đều lấy khóa ngay
  từ đầu, nên daemon, TUI, MCP và hook ghi song song mà không gặp lỗi "database is locked" giữa chừng.
- `taskpilot db stats | tasks | runs TASK | sql "SELECT …"` để xem dữ liệu (lệnh `sql` chạy ở chế
  độ `query_only`). Tab Status có mục **Thống kê**.
- Dữ liệu dạng file cũ đã được nhập vào DB (file gốc cất ở `tasks/<id>/legacy/`). Code nhập đã bỏ
  khỏi taskpilot (11/2026); muốn nhập lại thì dùng một bản taskpilot cũ hơn.

## Kế hoạch chuyển đổi từ file (đã làm bước 1–3)

1. Tạo package `internal/db` (mở kết nối, migration) và viết lại `store` trên
   SQLite, giữ nguyên các hàm public để daemon, TUI, MCP và hook không phải sửa.
2. Nhập dữ liệu cũ từ `~/.taskpilot/tasks/*` (đã chạy xong; lệnh `migrate-files` đã bỏ).
3. Thêm các phần mới: ghi `runs` và `tool_calls` từ stream-json, outbox có retry,
   `v_inbox` cho TUI.
4. Thêm phần secret: lệnh `taskpilot secret set|ref|bind|rotate|list` (không bao
   giờ in giá trị), `mcp-exec`, socket trong daemon, che secret trong log, rule
   chặn truy cập file của harness, và xóa khóa khỏi môi trường của `claude`.
