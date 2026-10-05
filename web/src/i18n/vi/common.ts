import type common from "../en/common";

// Vietnamese catalog: shared strings. Mirrors en/common.ts — same key set,
// enforced by the catalog parity test and lint:strings (V3.14).
export default {
  "common.cancel": "Huỷ",
  "common.close": "Đóng",
  "common.dismiss": "Bỏ qua",
  "common.toast.dismiss": "Tắt thông báo",
  "common.toast.errors": "Thông báo lỗi",
  "common.toast.notifications": "Thông báo",

  "errors.banner.fallback": "LỖI",
  "errors.banner.requestId": "(mã yêu cầu {id})",
  "errors.banner.retry": "Thử lại",
  "errors.banner.dismiss": "tắt lỗi",
  "errors.code.quotaExhausted":
    "Đã hết hạn mức — hãy xoá một workspace không dùng hoặc nhờ quản trị viên tăng hạn mức.",
  "errors.code.quotaReleasePending":
    "Một workspace vẫn đang tắt; hạn mức của nó được giải phóng trong khoảng 30 giây. Thử lại sau ít phút.",
  "errors.code.quotaNotConfigured":
    "Tenant của bạn chưa được cấu hình hạn mức. Nhờ quản trị viên thiết lập.",
  "errors.code.userLimitReached":
    "Bạn đã đạt giới hạn workspace đang chạy của mình ({current}/{limit}). Hãy dừng hoặc xoá một workspace, hoặc nhờ quản trị viên tăng giới hạn.",
  "errors.code.invalidTemplate":
    "Template đó không khả dụng (chưa phát hành hoặc không được phép cho tenant của bạn). Hãy chọn template khác.",
  "errors.code.idempotencyConflict":
    "Yêu cầu tạo xung đột với một lần thử trước đó. Kiểm tra danh sách trước khi thử lại.",
  "errors.code.invalidState":
    "Tài nguyên hiện không ở trạng thái cho phép thao tác này — có thể nó vừa thay đổi; làm mới rồi thử lại.",
  "errors.code.connectionInUse":
    "Một phiên khác đang kết nối tới workspace này.",
  "errors.code.imageStale":
    "Image runtime đã quá giới hạn độ mới. Nhờ quản trị viên làm mới image.",
  "errors.code.imageStaleNamed":
    "Image runtime của template {template} đã {ageDays} ngày tuổi — vượt giới hạn độ mới {limitDays} ngày. Nhờ quản trị viên làm mới image.",
  "errors.code.imageStalePinned":
    "Workspace này bị ghim vào revision hiện tại — chỉ khởi động lại được sau khi quản trị viên phát hành image mới.",
  "errors.code.forbidden": "Bạn không được phép thực hiện thao tác đó trên tài nguyên này.",
  "errors.code.notFound":
    "Tài nguyên này không tồn tại (hoặc bạn không thấy nó).",
  "errors.code.csrfFailed":
    "Token phiên của bạn đã hết hạn. Tải lại trang rồi thử lại.",
  "errors.code.unauthenticated":
    "Phiên của bạn đã hết hạn. Bạn sẽ được yêu cầu đăng nhập lại.",
  "errors.code.generic":
    "Dịch vụ không hoàn tất được yêu cầu; có thể thử lại an toàn.",
} satisfies Record<keyof typeof common, string>;
