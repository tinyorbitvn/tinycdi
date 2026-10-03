import type admin from "../en/admin";

// Vietnamese catalog: admin-area strings. Mirrors en/admin.ts (V3.14).
export default {
  // Section nav and access gate.
  "admin.nav.ariaLabel": "Mục quản trị",
  "admin.nav.overview": "Tổng quan",
  "admin.nav.workspaces": "Workspace",
  "admin.nav.quota": "Hạn mức",
  "admin.nav.templates": "Template",
  "admin.eyebrow": "Quản trị",
  "admin.eyebrow.tenant": "Quản trị · {tenant}",
  "admin.gate.checking": "Đang kiểm tra quyền",
  "admin.gate.title": "Chỉ quản trị viên tenant",
  "admin.gate.body":
    "Tài khoản của bạn không có vai trò quản trị viên tenant. Hỏi quản trị viên nếu bạn cần truy cập.",
  "admin.gate.back": "Về workspace của tôi",

  // Route titles (document title suffixes).
  "admin.route.overview": "Tổng quan tenant",
  "admin.route.workspaces": "Workspace của tenant",
  "admin.route.quota": "Hạn mức",
  "admin.route.templates": "Danh mục template",

  "admin.action.refresh": "Làm mới",

  // Overview page.
  "admin.overview.title": "Tổng quan tenant",
  "admin.overview.description": "Sức chứa và sức khoẻ của mọi workspace trong tenant.",
  "admin.overview.loading": "Đang tải tổng quan",
  "admin.overview.capacity": "Sức chứa",
  "admin.overview.usageByUser": "Mức dùng theo người dùng",
  "admin.overview.byPhase": "Workspace theo trạng thái",
  "admin.overview.allWorkspaces": "Tất cả workspace",
  "admin.overview.empty": "Chưa có workspace nào trong tenant này.",
  "admin.overview.failed.one": "{n} workspace lỗi",
  "admin.overview.failed.other": "{n} workspace lỗi",
  "admin.overview.failedAgo": "lỗi {age}",

  // Quota page.
  "admin.quota.title": "Hạn mức",
  "admin.quota.description": "Giới hạn tenant, mức dùng hiện tại và ai đang dùng.",
  "admin.quota.loading": "Đang tải hạn mức",
  "admin.quota.limits.title": "Giới hạn tenant",
  "admin.quota.limits.description": "Mức dùng hiện tại so với giới hạn của tenant {tenant}.",
  "admin.quota.users.title": "Mức dùng theo người dùng",
  "admin.quota.users.caption": "Mức dùng theo người dùng",
  "admin.quota.users.empty": "Chưa ghi nhận mức dùng nào.",
  "admin.quota.column.user": "Người dùng",
  "admin.quota.amount.workspaces": "Workspace",
  "admin.quota.amount.runningWorkspaces": "Workspace đang chạy",
  "admin.quota.amount.cpuMillicores": "CPU",
  "admin.quota.amount.memoryMib": "Bộ nhớ",
  "admin.quota.amount.storageGib": "Lưu trữ",
  "admin.quota.atLimit": "đã đạt giới hạn",
  "admin.quota.meter": "{used} / {limit} ({pct}%)",
  "admin.quota.notConfigured":
    "Chưa cấu hình hạn mức. Workspace mới bị từ chối cho tới khi tenant có hạn mức.",
  "admin.quota.noLimit": "Đã dùng {used} · Không giới hạn",
  "admin.quota.source.config": "Cấu hình nền tảng",
  "admin.quota.source.api": "API quản trị",
  "admin.quota.source.none": "Chưa cấu hình",
  "admin.quota.source.configNote":
    "Các hạn mức này được khai báo trong cấu hình nền tảng và chỉ có thể thay đổi ở đó.",
  "admin.quota.source.apiNote": "Các hạn mức này được đặt qua API quản trị.",
  "admin.quota.source.noneNote":
    "Chưa đặt hạn mức. Workspace mới sẽ bị từ chối cho đến khi hạn mức được lưu.",
  "admin.quota.edit.action": "Sửa hạn mức",
  "admin.quota.edit.title": "Đặt hạn mức tenant",
  "admin.quota.edit.description":
    "Được phép đặt hạn mức thấp hơn mức dùng hiện tại: các workspace đang chạy giữ nguyên phần đã đặt trước và workspace mới sẽ bị từ chối.",
  "admin.quota.edit.field.runningWorkspaces": "Workspace đang chạy",
  "admin.quota.edit.field.cpu": "CPU (vCPU)",
  "admin.quota.edit.field.memory": "Bộ nhớ (GiB)",
  "admin.quota.edit.field.storage": "Lưu trữ (GiB)",
  "admin.quota.edit.save": "Lưu hạn mức",
  "admin.quota.edit.cancel": "Huỷ",
  "admin.quota.edit.invalid":
    "Nhập số từ 0 trở lên; số workspace đang chạy và dung lượng lưu trữ phải là số nguyên.",

  // Template catalog page.
  "admin.templates.title": "Danh mục template",
  "admin.templates.description":
    "Các template được phát hành cho tenant này và số workspace dùng mỗi template. Template được phát hành dưới dạng tài nguyên WorkspaceTemplate bởi quản trị viên nền tảng.",
  "admin.templates.caption": "Danh mục template",
  "admin.templates.empty": "Chưa có template nào được phát hành cho tenant này.",
  "admin.templates.column.template": "Template",
  "admin.templates.column.kind": "Loại",
  "admin.templates.column.resources": "Tài nguyên",
  "admin.templates.column.image": "Image",
  "admin.templates.column.policy": "Chính sách",
  "admin.templates.column.lifecycle": "Không hoạt động / gia hạn / tối đa",
  "admin.templates.column.usage": "Workspace",
  "admin.templates.column.published": "Phát hành",
  "admin.templates.revision": "r{n}",
  "admin.templates.usageLabel": "{inUse} workspace, {running} đang chạy",
  "admin.templates.usage.running": "({n} đang chạy)",
  "admin.templates.badge.data": "Dữ liệu: {value}",
  "admin.templates.badge.clipboard": "Clipboard: {value}",
  "admin.templates.badge.network": "Mạng: {value}",
  "admin.templates.imageStale": "Cũ",
  "admin.templates.imageStaleHint": "Image runtime đã quá SLO độ mới.",
  "admin.templates.imageBlocked": "Bị chặn",
  "admin.templates.imageBlockedHint":
    "Image runtime vượt giới hạn chặn — tạo và khởi động bị từ chối (409 IMAGE_STALE).",
  "admin.templates.engine.chromium": "Chromium",
  "admin.templates.engine.firefox": "Firefox ESR",
  "admin.templates.imageUnknown": "Không rõ",

  // Tenant workspaces page.
  "admin.workspaces.title": "Workspace của tenant",
  "admin.workspaces.description": "Mọi workspace trong tenant, của tất cả người dùng.",
  "admin.workspaces.caption": "Workspace của tenant",
  "admin.workspaces.filter.label": "Lọc",
  "admin.workspaces.filter.placeholder": "Tên, chủ sở hữu, template hoặc ID",
  "admin.workspaces.phase.label": "Trạng thái",
  "admin.workspaces.phase.all": "Mọi trạng thái",
  "admin.workspaces.truncated":
    "Đang hiển thị {n} workspace đầu tiên; thu hẹp bộ lọc trạng thái để xem phần còn lại.",
  "admin.workspaces.empty.filtered": "Không có workspace nào khớp bộ lọc.",
  "admin.workspaces.empty.all": "Chưa có workspace nào trong tenant này.",
  "admin.workspaces.shown": "Hiển thị {shown} trên {total} workspace",
  "admin.workspaces.column.workspace": "Workspace",
  "admin.workspaces.column.owner": "Chủ sở hữu",
  "admin.workspaces.column.template": "Template",
  "admin.workspaces.column.phase": "Trạng thái",
  "admin.workspaces.column.age": "Tuổi",
  "admin.workspaces.column.actions": "Thao tác",
  "admin.workspaces.action.stop": "Dừng",
  "admin.workspaces.action.delete": "Xoá",
  "admin.workspaces.action.stopLabel": "Dừng {name}",
  "admin.workspaces.action.deleteLabel": "Xoá {name}",
  "admin.workspaces.notice.stop": "Đã yêu cầu dừng {name} ({owner}).",
  "admin.workspaces.notice.delete": "Đã yêu cầu xoá {name} ({owner}).",
  "admin.workspaces.stop.title": "Dừng {name}?",
  "admin.workspaces.stop.fallbackTitle": "Dừng workspace",
  "admin.workspaces.stop.confirm": "Dừng workspace",
  "admin.workspaces.stop.body":
    "Workspace thuộc về {owner}. Dừng nó sẽ kết thúc ngay mọi phiên đang mở; công việc chưa lưu trong desktop sẽ mất.",
  "admin.workspaces.stop.retain": "Đĩa của nó được giữ và chủ sở hữu có thể khởi động lại.",
  "admin.workspaces.stop.ephemeral": "Dữ liệu của nó là tạm thời và bị huỷ khi dừng.",
  "admin.workspaces.delete.title": "Xoá {name}?",
  "admin.workspaces.delete.fallbackTitle": "Xoá workspace",
  "admin.workspaces.delete.confirm": "Xoá workspace",
  "admin.workspaces.delete.body":
    "Thao tác này gỡ workspace của {owner} và thu hồi quyền truy cập.",
  "admin.workspaces.delete.retain":
    "Đĩa của nó chuyển sang kho dữ liệu giữ lại; xoá vĩnh viễn ở đó để huỷ nó.",
  "admin.workspaces.delete.ephemeral": "Dữ liệu của nó là tạm thời và sẽ bị huỷ.",
  "admin.workspaces.delete.typeConfirm": "Gõ {name} để xác nhận",
  "admin.workspaces.dialog.cancel": "Huỷ",

  // Error guidance (stable code -> next step; server messages are detail).
  "admin.errors.title": "Lỗi",
  "admin.errors.retry": "Thử lại",
  "admin.errors.requestId": "Mã yêu cầu: {id}",
  "admin.errors.forbidden":
    "Bạn không có quyền thực hiện việc này. Các chế độ xem toàn tenant cần vai trò quản trị viên tenant.",
  "admin.errors.notFound": "Nó không còn tồn tại hoặc bạn không thấy nó. Làm mới danh sách.",
  "admin.errors.invalidState":
    "Trạng thái của nó đã thay đổi và không còn cho phép việc này. Làm mới rồi thử lại khi nó ổn định.",
  "admin.errors.invalidRequest":
    "Yêu cầu bị từ chối. Nếu bạn đang xác nhận một lần xoá vĩnh viễn, mở lại hộp thoại để có xác nhận mới.",
  "admin.errors.invalidTemplate":
    "Template đó không khả dụng hoặc không khớp runtime của đĩa. Chọn template khác.",
  "admin.errors.quotaExhausted": "Hết hạn mức. Giải phóng tài nguyên hoặc tăng hạn mức trước.",
  "admin.errors.quotaNotConfigured": "Tenant của bạn chưa được cấu hình hạn mức. Nhờ quản trị viên thiết lập.",
  "admin.errors.quotaManagedByConfig":
    "Hạn mức của tenant này do cấu hình nền tảng quản lý và không thể thay đổi ở đây.",
  "admin.errors.preconditionFailed":
    "Có người khác vừa thay đổi các hạn mức này. Giá trị hiện tại đã được tải lại — hãy kiểm tra rồi lưu lại.",
  "admin.errors.idempotencyConflict":
    "Yêu cầu này xung đột với một lần thử trước. Làm mới và kiểm tra trước khi thử lại.",
  "admin.errors.csrfFailed": "Token phiên của bạn đã hết hạn. Tải lại trang rồi thử lại.",
  "admin.errors.unauthenticated": "Phiên của bạn đã hết hạn. Tải lại trang để đăng nhập lại.",
  "admin.errors.default": "Dịch vụ không hoàn tất được yêu cầu. Có thể thử lại an toàn.",

  // Compact time and unit formats.
  "admin.time.justNow": "vừa xong",
  "admin.time.ago": "{age} trước",
  "admin.time.s": "{n} giây",
  "admin.time.m": "{n} phút",
  "admin.time.h": "{n} giờ",
  "admin.time.hm": "{h} giờ {m} phút",
  "admin.time.d": "{n} ngày",
  "admin.time.dh": "{d} ngày {h} giờ",
  "admin.unit.vcpu": "{n} vCPU",
  "admin.unit.mib": "{n} MiB",
  "admin.unit.gib": "{n} GiB",
  "admin.unit.tib": "{n} TiB",
} satisfies Record<keyof typeof admin, string>;
