import type session from "../en/session";

// Vietnamese catalog: session-area strings. Mirrors en/session.ts (V3.14).
export default {
  "session.page.ariaLabel": "Phiên desktop: {name}",

  "session.toolbar.back": "Về bảng điều khiển",
  "session.toolbar.controls": "Điều khiển phiên",
  "session.toolbar.reconnect": "Kết nối lại",
  "session.toolbar.openInNewTab": "Mở trong tab mới",
  "session.toolbar.fullscreen": "Toàn màn hình",
  "session.toolbar.exitFullscreen": "Thoát toàn màn hình",
  "session.toolbar.printHint": "In và tải xuống cần \"Mở trong tab mới\".",

  "session.status.loading": "Đang tải",
  "session.status.notReady": "Chưa sẵn sàng",
  "session.status.starting": "Đang khởi động",
  "session.status.requesting": "Đang kết nối",
  "session.status.inUse": "Đang dùng ở nơi khác",
  "session.status.connecting": "Đang kết nối",
  "session.status.connected": "Đã kết nối",
  "session.status.reconnecting": "Đang kết nối lại",
  "session.status.disconnected": "Mất kết nối",
  "session.status.ended": "Đã kết thúc",
  "session.status.blocked": "Bị chặn",
  "session.status.external": "Ở tab khác",
  "session.status.elsewhere": "Mở ở tab khác",
  "session.status.error": "Lỗi",
  "session.status.signedOut": "Đã đăng xuất",

  "session.frame.title": "Desktop: {name}",
  "session.frame.keyboardHint":
    "Bàn phím nhập vào desktop khi desktop đang được chọn. Dùng phím tắt của trình duyệt hoặc nhấp ra ngoài desktop để thoát.",

  "session.progress.loading": "Đang tải workspace…",
  "session.progress.requesting": "Đang yêu cầu phiên bảo mật…",
  "session.progress.connecting": "Đang kết nối tới desktop…",
  "session.progress.newTabHint": "Lâu quá?",
  "session.progress.newTabAction": "Mở nó trong tab mới",

  "session.inUse.title": "Workspace này đang mở ở nơi khác",
  "session.inUse.body":
    "Một phiên khác đang kết nối tới workspace này. Tiếp quản sẽ ngắt kết nối của phiên đó.",
  "session.inUse.takeover": "Tiếp quản phiên",

  "session.elsewhere.title": "Phiên này đang mở ở tab khác",
  "session.elsewhere.body":
    "Một tab hoặc cửa sổ khác đã mở desktop này. Dùng ở đây sẽ ngắt kết nối bên kia.",
  "session.elsewhere.useHere": "Dùng ở đây",

  "session.notReady.title": "Workspace này chưa sẵn sàng kết nối",
  "session.notReady.fallback": "Khởi động workspace rồi thử lại.",

  "session.disconnected.title": "Mất kết nối",
  "session.disconnected.offline":
    "Thiết bị của bạn đã ngoại tuyến. Kết nối lại khi mạng trở lại.",
  "session.disconnected.unreachable":
    "Cổng hiện không kết nối được tới dịch vụ. Kết nối lại để thử.",
  "session.disconnected.exhausted":
    "Phiên không khôi phục được tự động. Kết nối lại để thử.",
  "session.disconnected.reconnect": "Kết nối lại",

  "session.ended.title": "Phiên đã kết thúc",
  "session.ended.stopped": "Workspace đã bị dừng. Khởi động nó để kết nối lại.",
  "session.ended.failed": "Workspace bị lỗi. Mở chi tiết để xem chuyện gì đã xảy ra.",
  "session.ended.deleted": "Workspace đã bị xoá.",
  "session.ended.unknown": "Workspace không còn chạy.",

  "session.blocked.title": "Không hiển thị được desktop trong cổng",
  "session.blocked.timeout":
    "Phiên không tải kịp — trình duyệt hoặc mạng của bạn có thể đang chặn phiên nhúng.",
  "session.blocked.policy": "Trình duyệt của bạn đã chặn phiên nhúng.",
  "session.blocked.newTabHint": "Bạn có thể mở nó trong tab mới.",
  "session.blocked.retry": "Thử lại tại đây",

  "session.external.title": "Phiên đã mở trong tab mới",
  "session.external.body": "Đưa nó về đây sẽ ngắt kết nối của tab kia.",
  "session.external.showHere": "Hiển thị ở đây",

  "session.error.title": "Không khởi động được phiên",
  "session.error.retry": "Thử lại",
  "session.error.requestId": "Mã yêu cầu: {id}",
  "session.error.generic": "Có lỗi xảy ra.",
  "session.error.api.invalidState":
    "Workspace hiện không nhận kết nối — có thể nó đang khởi động hoặc đang dừng.",
  "session.error.api.forbidden": "Bạn không được phép kết nối tới workspace này.",
  "session.error.api.notFound": "Workspace này không tồn tại (hoặc bạn không thấy nó).",
  "session.error.api.rateLimited": "Quá nhiều lần thử kết nối. Chờ một lát rồi thử lại.",
  "session.error.api.unauthenticated":
    "Đăng nhập của bạn đã hết hạn. Tải lại trang để đăng nhập lại.",
  "session.error.api.csrfFailed": "Token phiên của bạn đã hết hạn. Tải lại trang rồi thử lại.",
  "session.error.api.fallback":
    "Dịch vụ không khởi động được phiên; có thể thử lại an toàn.",

  "session.signedOut.title": "Bạn đã bị đăng xuất",
  "session.signedOut.body":
    "Đăng nhập của bạn đã hết hạn nên cổng không còn kết nối được tới phiên của bạn. Đăng nhập lại để đưa desktop trở lại.",
  "session.signedOut.action": "Đăng nhập lại",

  "session.overlay.workspaceDetails": "Chi tiết workspace",

  "session.clipboard.label": "Clipboard",
  "session.clipboard.disabled":
    "Chia sẻ clipboard bị tắt cho workspace này theo chính sách.",
  "session.clipboard.hint1":
    "Sao chép và dán giữa máy tính này và desktop bằng phím tắt thông thường khi desktop đang được chọn.",
  "session.clipboard.hint2":
    "Trình duyệt có thể hỏi quyền truy cập clipboard cho phiên — hãy cho phép để dán từ máy tính này.",

  "session.lifecycle.stopsAfter": "Dừng sau {duration} không hoạt động",
  "session.lifecycle.endsBy": "kết thúc trước {time}",
  "session.lifecycle.warn":
    "Kết thúc sau {minutes} phút — hãy lưu công việc.",

  "session.blocker.stopped": "Workspace đã dừng.",
  "session.blocker.phase": "Workspace đang ở trạng thái {phase}. Thử lại khi nó sẵn sàng.",
  "session.blocker.connection": "Điểm truyền phát của desktop chưa sẵn sàng.",
} satisfies Record<keyof typeof session, string>;
