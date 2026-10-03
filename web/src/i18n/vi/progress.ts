import type progress from "../en/progress";

// Vietnamese catalog: lifecycle progress strings. Mirrors en/progress.ts
// (V3.14).
export default {
  "progress.create.title": "Đang tạo {name}",
  "progress.start.title": "Đang khởi động {name}",
  "progress.stop.title": "Đang dừng {name}",
  "progress.delete.title": "Đang xoá {name}",

  "progress.step.accepted": "Đã chấp nhận yêu cầu",
  "progress.step.queued": "Đang chờ tới lượt",
  "progress.step.scheduling": "Đang lập lịch khởi động",
  "progress.step.disk": "Đang chuẩn bị đĩa",
  "progress.step.diskAttach": "Đang gắn đĩa của bạn",
  "progress.step.machine": "Đang tìm máy",
  "progress.step.machinePrepare": "Đang chuẩn bị máy",
  "progress.step.imagePull": "Đang tải image desktop",
  "progress.step.desktop": "Đang chuẩn bị desktop",
  "progress.step.desktopStart": "Đang khởi động desktop",
  "progress.step.connect": "Sẵn sàng kết nối",
  "progress.step.shutdown": "Đang tắt",
  "progress.step.stopped": "Đã dừng",
  "progress.step.sessions": "Đang đóng các phiên",
  "progress.step.runtime": "Đang gỡ desktop",
  "progress.step.data": "Đang xử lý dữ liệu của bạn",
  "progress.step.finishing": "Đang hoàn tất",
  "progress.step.working": "Đang xử lý",

  "progress.state.done": "xong",
  "progress.state.active": "đang chạy",
  "progress.state.waiting": "đang chờ",
  "progress.state.failed": "lỗi",
  "progress.state.skipped": "đã bỏ qua",

  "progress.step.of": "bước {current} trên {total}",
  "progress.step.elapsed": "trong {elapsed}",
  "progress.step.reason": "Trạng thái: {reason}",

  "progress.slow.machine": "Hiện không có máy nào còn chỗ. Hệ thống vẫn đang thử.",
  "progress.slow.imagePull":
    "Image desktop vẫn đang tải. Lần khởi động đầu có thể mất vài phút.",
  "progress.slow.drain": "Đang chờ các phiên mở đóng (tối đa 45 giây).",
  "progress.slow.generic": "Việc này đang lâu hơn bình thường.",

  "progress.failed.imagePull":
    "Không tải được image desktop. Thử khởi động lại; nếu lặp lại, liên hệ quản trị viên.",
  "progress.failed.deadline": "Workspace không sẵn sàng kịp thời.",
  "progress.failed.cleanup": "Một bước dọn dẹp lỗi và sẽ được thử lại.",
  "progress.failed.generic": "Bước này lỗi.",

  "progress.notice.retry": "Nền tảng gặp lỗi và đang thử lại.",

  "progress.refresh.retrying": "Không làm mới được tiến trình, đang thử lại…",
  "progress.delayed": "Trạng thái bị trễ. Đang hiển thị bước gần nhất đã biết.",
  "progress.stalled":
    "Vẫn đang chờ. Không có gì sai ở phía bạn. Liên hệ quản trị viên.",

  "progress.deleted.toast": "'{name}' đã bị xoá.",
  "progress.deleted.retainedLink": "Đĩa của bạn nằm trong Dữ liệu giữ lại",
  "progress.deleted.announce": "{name} đã bị xoá.",
} satisfies Record<keyof typeof progress, string>;
