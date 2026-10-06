import type app from "../en/app";

// Vietnamese catalog: app-shell strings. Mirrors en/app.ts (V3.14).
// Language names stay self-named ("English", "Tiếng Việt") in both locales.
export default {
  "app.notFound.title": "Không tìm thấy trang",
  "app.notFound.body": "Địa chỉ {path} không khớp trang nào trong cổng này.",
  "app.notFound.action": "Đến trang workspace",
  "app.loadError.title": "Không tải được trang này",
  "app.loadError.body": "Tải lại trang để thử lại.",

  "app.shell.byline": "bởi TinyOrbit",
  "app.theme.label": "Giao diện",
  "app.theme.light": "Sáng",
  "app.theme.dark": "Tối",
  "app.theme.system": "Theo hệ thống",

  "app.language.label": "Ngôn ngữ",
  "app.language.en": "English",
  "app.language.vi": "Tiếng Việt",

  "app.user.menu": "Menu tài khoản của {name}",
  "app.user.signedInAs": "Đã đăng nhập là {name}",
  "app.user.tenant": "Tenant {tenant}",
  "app.user.signOut": "Đăng xuất",
  "app.user.signOutAll": "Đăng xuất khỏi mọi nơi",
  "app.user.signOutAll.title": "Đăng xuất khỏi mọi nơi?",
  "app.user.signOutAll.body":
    "Thao tác này kết thúc mọi phiên của bạn trong tenant {tenant} — trên tất cả trình duyệt và thiết bị, kể cả thiết bị này. Các phiên desktop đang chạy sẽ đóng trong vài giây. Phiên ở tenant khác không bị ảnh hưởng.",
  "app.user.signOutAll.confirm": "Đăng xuất khỏi mọi nơi",
  "app.user.signOutAll.busy": "Đang đăng xuất…",
  "app.user.signOutFailed.title": "Không đăng xuất được",
  "app.user.signOutFailed.body":
    "Bạn vẫn đang đăng nhập. Thử lại — nếu vẫn lỗi, hãy đóng cửa sổ trình duyệt này.",

  "auth.signedOut.title": "Bạn đã đăng xuất",
  "auth.signedOut.body": "Phiên của bạn đã kết thúc. Đóng cửa sổ này hoặc đăng nhập lại.",
  "auth.signedOut.action": "Đăng nhập lại",

  "auth.gate.apiUnreachable": "Không kết nối được tới API workspace: {error}",
  "auth.gate.checking": "Đang kiểm tra phiên…",
  "auth.gate.retrying": "Không kết nối được tới API workspace ({error}). Đang thử lại…",

  "nav.sections": "Mục",
  "nav.workspaces": "Workspace",
  "nav.admin": "Quản trị",
  "nav.newWorkspace": "Workspace mới",
  "nav.templates": "Danh mục template",
  "nav.data": "Dữ liệu giữ lại",
  "nav.allWorkspaces": "Tất cả workspace",
} satisfies Record<keyof typeof app, string>;
