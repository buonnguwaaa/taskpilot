# BOM — log production

Nguồn: `bom-base` (kho kiến thức BOM) và code các service.

## Phạm vi trên Splunk

- **Namespace prod: chưa xác minh.** Chạy truy vấn "Tìm namespace & container
  của BOM" (tab Logs Production), rồi đặt `logs_production.filter` của project
  `bom` trong config.
- **Container** dự kiến trùng tên service: `bom-gateway`, `bom-api`,
  `bom-worker`, `bom-report`, `bom-web-socket`.
- **Không có log trên cluster:** `bom-bartender-app` (chạy local ở máy in),
  `bom-base` (tài liệu), `bom-frontend` (log ở trình duyệt).

## Định dạng log

- Mọi service Go của BOM dùng **zap** (`go.uber.org/zap`); `bom-report` ghi
  thêm file qua Lumberjack. Trường thường gặp: `level`, `ts`, `msg`/`message`, `caller`.

## Nguồn khác ngoài Splunk

- Mọi request 4xx/5xx được middleware Tracking ghi bất đồng bộ vào
  **Elasticsearch index `bom_request_logs`**
  (`package/middleware/tracking.go`, xem `bom-base/tech/flows/auth-rbac.md`).
  Khi cần danh sách request lỗi theo user/endpoint, đây là nguồn đầy đủ hơn log.
- Elasticsearch còn lưu action log của người dùng.

## Luồng (để biết tìm ở container nào)

`bom-frontend` → `bom-gateway` (REST, JWT, RBAC) → `bom-api` (gRPC, 28 module);
việc nền chạy ở `bom-worker` (Kafka + cron + asynq); xuất Excel ở `bom-report`;
realtime qua `bom-web-socket` (Redis Pub/Sub).

## Cách điều tra

- **Lỗi màn hình / API cụ thể:** `kubernetes.container_name=bom-gateway "<path>"`,
  rồi theo sang `bom-api` cùng khoảng thời gian.
- **Job nền / đồng bộ lỗi:** `kubernetes.container_name=bom-worker level=error`.
- **Lỗi tăng đột biến:** `level=error | stats count by kubernetes.container_name`.
