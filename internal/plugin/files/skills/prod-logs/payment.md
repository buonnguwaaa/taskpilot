# Payment — log production

Nguồn: cách tìm log thực tế bằng `payment/scripts/splunk-log-audit`,
`payment-wiki/docs/operations/observability.md`, `payment-wiki/docs/known-issues.md`
và `payment/CLAUDE.md`.

## Phạm vi trên Splunk

- **Namespace prod:** `kubernetes.namespace_name=payment-prod` (đã dùng nhiều lần).
- **Container** trùng tên service. Đã xác nhận: `payment`, `payment-techcom`,
  `payment-fiserv`, `payment-stripe`. Các service khác (`payment-worker`,
  `payment-zalopay`, `payment-vnpay`, gateway, auth) theo cùng quy ước nhưng
  **chưa xác minh tên**: chạy truy vấn "Liệt kê container" để biết chính xác.
- **Index:** không chỉ định (dùng index mặc định của tài khoản).
- **Khoảng thời gian thường dùng:** `-1h` khi theo dõi, `-24h` khi điều tra,
  tối đa `-168h` (7 ngày).

## Định dạng log

- Fluent Bit gói mỗi dòng stdout trong envelope JSON: `log`, `kubernetes.pod_name`,
  `kubernetes.container_name`, `time`.
- Log app trên prod là **zap JSON**: `message`, `ts`, `level`, `caller`,
  `stacktrace`; thêm `prefix` (tên hàm, từ `logger.WithPrefix`) và `request_id`.
  `payment_auth` dùng logger của zeus.
- Các truy vấn mẫu của team dùng trực tiếp `level=error`, `request_id=<id>`.
- Nhiễu thường gặp (không phải zap JSON): SQL của GORM in ra stdout (nhiều
  dòng, có mã màu ANSI, không có `request_id`), `fmt.Print*`, zap console
  encoder, dòng trống. Script `scripts/splunk-log-audit/check_log_format.py`
  phân loại các loại này.

## `request_id` — khi nào có, khi nào không

- `payment_gateway`: đọc `X-Request-ID` hoặc sinh uuid, trả về trong response
  header, forward xuống backend.
- Access log `API Request` của gateway: **luôn rỗng** (bug: đọc sai key), đừng
  tìm request theo dòng này.
- `payment` và 5 provider (stripe, techcom, zalopay, vnpay, fiserv): có, qua
  interceptor gRPC.
- Kafka handler (`payment`, worker): **không có** (ctx mới).
- `payment-worker` (asynq, gọi gRPC): **không có**; provider nhận call từ
  worker tự sinh uuid mới.
- `payment_auth`: không có.

⇒ Request từ client: tra theo `request_id`. Luồng do worker/Kafka khởi tạo:
tra theo `transaction_code` / `refund_code` / `order_code` (tìm như chuỗi).

## Luồng request (để biết tìm ở container nào)

1. Client → `payment_gateway` (REST, xác thực JWT).
2. Gateway → `payment` (transaction) và các provider gRPC.
3. `payment` ghi MySQL + outbox → Debezium → Kafka `cdc-es-event`.
4. `payment-worker` tiêu thụ Kafka và chạy job Asynq gọi lại provider để
   poll / cancel / refund.
5. Provider nói chuyện với PSP và chuẩn hoá trạng thái (`MapTransactionStatus`).

## Log có cấu trúc của worker (timeout scan)

- Mỗi lần chạy: `since`, `source_systems`, `allow_cancel`.
- Mỗi record: `age`, `timed_out`.
- `cancel transaction response`: `transaction_code`, `success`, `status`, `return_code`.
- Ngưỡng cancel hiệu lực **không** được log.

## Lỗ hổng đã biết

- `payment-zalopay` mất khoảng 5% log trên Splunk (known-issues AI-12).
- Không có tracing xuyên service; metrics chỉ có ở `payment-fiserv` (chưa scrape).
- Bổ sung cho log: bảng MySQL `provider_requests` ghi mỗi lần gọi PSP hoặc
  nhận callback (`object_code` = transaction/refund code, `type` = order /
  refund / check_transaction / callback / …). Lỗi ghi bảng này chỉ log warn,
  nên thiếu row cũng không chứng minh là không có call.

## Cách điều tra

- **Có `transaction_code` / mã đơn:** tìm `"<code>"` trong cả namespace
  (`-24h` đến `-168h`), xem container nào có log, rồi lọc theo container đó.
- **Có `request_id`** (header `X-Request-ID`): `request_id=<id>` trong cả namespace.
- **Lỗi tăng đột biến:** `level=error | stats count by kubernetes.container_name`
  (`-1h`), rồi xem lỗi của container nhiều nhất.
- **TCB không bắn callback / giao dịch treo:**
  `kubernetes.container_name=payment-techcom "<transaction_code>"`, rồi đối chiếu
  `provider_requests` (`type='callback'`) và log cancel của worker.
- **Đơn bị huỷ do quá hạn:**
  `kubernetes.container_name=payment-worker "cancel transaction response" "<transaction_code>"`.
