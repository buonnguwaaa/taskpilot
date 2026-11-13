# Hasaki Work API (khám phá ngày 2026-11-09)

Không có tài liệu chính thức. Thông tin dưới đây lấy từ traffic của web app
`work.hasaki.vn` (Next.js) khi mở trang /tasks, và từ code JS của trang. Chỉ các
endpoint **đọc** đã được gọi thử; endpoint **ghi** chỉ được đọc trong code, chưa
gọi thật lần nào.

## Xác thực

- Đăng nhập qua SSO (OIDC) trên `work.hasaki.vn`. Sau khi đăng nhập, trình duyệt
  giữ cookie httpOnly **`wshr-token`**. Cookie này **chính là JWT**, sống 48 giờ,
  và bản thân cookie hết hạn sau 7 ngày.
- `GET https://work.hasaki.vn/api/auth/token` (gửi kèm cookie) trả về
  `{"token": "<JWT>"}`. Đây là cách web app lấy JWT.
- Mọi lời gọi API gửi kèm `Authorization: Bearer <JWT>` và `X-Locale: vi|en`.
- JWT có thể bị server **blacklist** trước khi hết hạn (đã xảy ra với token dán
  tay: `{"message":"The token has been blacklisted","status":0}`). Vì vậy
  taskpilot dùng phiên riêng của nó (`taskpilot auth login`).
- Lúc ghi tài liệu này, **chưa rõ** khi JWT trong cookie hết hạn sau 48 giờ thì
  server có tự gia hạn không. Nếu không, `auth status` sẽ báo hết hạn và phải
  chạy lại `auth login`.

## Base URL và dạng response

`https://wshr.hasaki.vn/api` (môi trường staging: `https://wshr.inshasaki.com/api`).

Có hai dạng response:
`{"message":"Success","status":1,"code":200,"data":…}` và `{"status":200,"message":"OK","data":…}`.

## Endpoint

| Mục đích | Request | Ghi chú |
|---|---|---|
| User hiện tại | `GET setting/user/profile?employee=1` | `data.profile.{id,name,email}` |
| Task của tôi | `GET hr/projects/mytasks-es?option=my-task&status=_00_01&limit=100&offset=0&to_date=YYYY-MM-DD` | `data` là mảng; không có mô tả |
| Đếm task theo trạng thái | `GET hr/projects/aggregations?option=my-task&to_date=…` | các khóa `_00`, `_01`, `_02`, `_04`, `_05`, `lated` |
| Chi tiết task | `GET hr/projects/task-input/{id}` | mô tả nằm trong **`note`** (markdown) |
| Comment | `GET v2/task/comment?obj_id={id}` | `data` là mảng (có lúc là `{rows}`) |
| Thêm comment ✍️ | `POST v2/task/comment`, multipart: `user_id`, `obj_id`, `type=2`, `comment`, `parent_id`?, `file[]`? | chưa gọi thử |
| Đổi trường task ✍️ | `POST hr/projects/update-field`, multipart: `id`, `field=status`, `value` | chưa gọi thử |
| Đổi trạng thái assignment ✍️ | `POST hr/projects/update-assign-status`, JSON: `task_id`, `assign_id` (id của dòng `staff[]` của task, không phải staff_id), `status` (2 xong, 5 chờ duyệt) | theo code JS của web; server từ chối "hoàn thành" khi task chưa có kết quả công việc ("Vui lòng cập nhật kết quả công việc để hoàn thành!"). Chưa gọi thật |
| Ghi kết quả công việc ✍️ | `POST hr/projects/mass-update-field-task-input`, multipart: `id`, `field=data`, `value[configs][virtual_text]` (HTML) | theo code JS của web (ô "Kết quả công việc"); task sub_type single dùng `update-assign-field` thay vì đây. Chưa gọi thật |
| Tạo task ✍️ | `POST hr/projects/upsert-task-input/0`, JSON: `id=0`, `name`, `prid`, `parent_id` (EPIC), `milestone_id` (sprint, lấy từ EPIC như web làm khi tạo subtask), `planned_hours` (phút, 480 = 8h), `date_end` (`YYYY-MM-DD 00:00:00`), `note` (mô tả), `no_assign=1`, `sub_type=0` | đã gọi thật 2026-11-10: `data` là task mới (`id`, `code`, `planned_hours` dạng chuỗi), `assign_to_user: []` (chưa giao) |
| Giao người ✍️ | `POST hr/projects/mass-update-field-task-input`, multipart: `id`, `field=assign_staff`, `value[]=<staff_id>` | theo code JS của web (ô chọn người thực hiện); `staff_id` là `staff_info.staff_id` của profile, không phải user id |
| Task của một dự án | `GET hr/projects/mytasks-es?option=all&prid={id}&limit=500&offset=0[&status=…][&keyword=…]` | đã gọi thử 2026-11-10; `keyword` lọc theo tên. Index **không trả đủ**: một số EPIC đang mở chỉ thấy qua `parent` của task con |
| Thông tin dự án | `GET hr/projects/{prid}` | đã gọi thử |
| Activity | `GET hr/projects/activities` | chưa thử |

Bộ lọc trạng thái (`status=`): `_00_01` (đang mở), `_05` (chờ duyệt), `_02` (đã
xong), `_04` (đã hủy).

Mã trạng thái (`STATUS_TASK_*`): 0 = mới, 1 = đang làm, 2 = xong, 3 = trễ hạn,
4 = hủy, 5 = chờ duyệt, 6 = thất bại.

Các trường chính của một task: `id`, `code` (ví dụ `HSK-9602DG23`), `name`, `note`,
`status`, `prid` cùng `project{id,name}`, `parent{id,name,code}` (thường là EPIC),
`milestone{id,name}` (sprint), `date_end`, `planned_hours` (tính bằng phút?),
`created_by_user`, `assign_to_user[]`, `tags[]`.

## Tệp đính kèm

File đính kèm mô tả nằm trong `note_files[]` (chỉ có ở `task-input/{id}`). Đây là
đường dẫn tương đối, ví dụ `task/26/11/12/15/Flow_2026-11-12_152833.png`. URL đầy
đủ là `https://hr-media.hasaki.vn/production/hr/<path>` (web app ghép
`getMediaUrl() + "/production/hr/"`). URL này công khai, không cần token. Server
thêm hậu tố `_YYYY-MM-DD_HHMMSS` vào tên file. Khi giao task cho agent
(phím Enter ở tab Hasaki Work), taskpilot tải các file này về `refs/hw-<code>/`
cạnh task và liệt kê chúng trong đề bài.

Ảnh kết quả QC của các bước workflow nằm ở
`subtasks[].data.configs.virtual_media[]`, cùng host media. taskpilot chưa tải các ảnh này.

Lúc khám phá, có 24 task đang mở thuộc 3 project: BOM (Manufactory) `8388`,
PLM `7264`, Payment (New) `7771`.

## EPIC theo sprint

Mỗi dự án có một EPIC cho mỗi loại trong mỗi sprint: `[EPIC] DEV …` (hoặc
`DEV & QC`), `[EPIC] Improve Sprint N`, `[EPIC] Issue Production`. EPIC gắn với
sprint qua `milestone_id`. Phím `h` (tab Tasks) và `taskpilot hw epics` tìm EPIC
đang mở mới nhất của từng loại: ứng viên lấy từ `keyword=EPIC`, từ `parent` của
task đang mở trong dự án và của task của tôi; mỗi ứng viên được đọc chi tiết
(`task-input/{id}`) để biết trạng thái và sprint. Bỏ qua EPIC đã xong (2) hoặc hủy (4).

`milestone_id` có lúc là số, có lúc là chuỗi hoặc `null`.

Để khám phá thêm: `taskpilot hw get PATH k=v …` gọi GET và in `data`.
