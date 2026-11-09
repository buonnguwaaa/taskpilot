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
| Đổi trạng thái assignment ✍️ | `POST hr/projects/update-assign-status`: `task_id`, `assign_id`, `status` | chưa gọi thử |
| Activity | `GET hr/projects/activities` | chưa thử |

Bộ lọc trạng thái (`status=`): `_00_01` (đang mở), `_05` (chờ duyệt), `_02` (đã
xong), `_04` (đã hủy).

Mã trạng thái (`STATUS_TASK_*`): 0 = mới, 1 = đang làm, 2 = xong, 3 = trễ hạn,
4 = hủy, 5 = chờ duyệt, 6 = thất bại.

Các trường chính của một task: `id`, `code` (ví dụ `HSK-9602DG23`), `name`, `note`,
`status`, `prid` cùng `project{id,name}`, `parent{id,name,code}` (thường là EPIC),
`milestone{id,name}` (sprint), `date_end`, `planned_hours` (tính bằng phút?),
`created_by_user`, `assign_to_user[]`, `tags[]`.

Lúc khám phá, có 24 task đang mở thuộc 3 project: BOM (Manufactory) `8388`,
PLM `7264`, Payment (New) `7771`.
