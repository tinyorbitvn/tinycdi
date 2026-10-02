# ADR 0001: Hai đường streaming runtime — KasmVNC cho Linux/browser, Guacamole/RDP cho Windows
> **English note:** this document is in Vietnamese — it is the original design/ADR kept for reference. Current platform state is described by `README.md`, `docs/images.md`, `docs/compatibility.md` and `docs/runbooks/`.


Ngày: 2026-09-29. Đối chiếu thực nghiệm: 2026-09-30.
Trạng thái: **Accepted** — mọi contract Linux đã đối chiếu và có evidence; verdict "PASS with conditions" đã được chốt 2026-09-30. Phạm vi chứng minh hiện tại là đường Linux trên một cluster Kubernetes tham chiếu; đường Windows có gate chứng minh riêng và hiện deferred vì chưa có source image. Bảng đối chiếu evidence ở cuối tài liệu — mọi claim cốt lõi đã được gắn trạng thái PROVEN/STATIC/VENDOR.

*(Ghi chú: mọi claim cốt lõi dưới đây đã được kiểm chứng và giữ trạng thái PROVEN/STATIC/VENDOR.)*

## Bối cảnh

Yêu cầu: Linux desktop/browser và Windows desktop trên Kubernetes, một user một kết nối tương tác được cấp quyền tại một thời điểm, vượt 5 session đồng thời không phụ thuộc Kasm Workspaces CE. KasmVNC định hướng Linux và có protocol riêng khác VNC/RFB truyền thống; Windows desktop thực tế cần RDP với NLA. Hai workload có cơ chế streaming khác nhau nên một đường duy nhất sẽ phải port KasmVNC sang Windows hoặc viết lại RDP — cả hai đều tốn kém và rủi ro.

## Quyết định

1. **Linux/browser:** Pod chạy KasmVNC upstream (pin version/digest), đứng sau authenticated reverse proxy (Session Gateway). Gateway inject credential server-side; browser không giữ runtime credential.
   - *Evidence đã chỉnh:* KasmVNC 1.5.0 chỉ có WebSocket transport tại `/websockify` — không tồn tại HTTP tunnel fallback upstream nào để test (PROVEN). Đường Guacamole deferred (mục 2) là stack riêng, không liên quan và không bị ảnh hưởng bởi phát hiện này.
2. **Windows (deferred):** KubeVirt VM + RDP/NLA qua Apache Guacamole + guacd, custom Java authentication extension lấy connection đã cấp quyền từ Broker qua mTLS. Không port KasmVNC sang Windows, không viết lại RDP hay Guacamole tunnel protocol.
3. **Authorization contract chung:** `POST /v1/workspaces/{id}/connections` cấp opaque ticket 60s một lần; gateway redeem qua Broker và nhận lease ràng **(workspaceUID, runtimeGeneration, runtimeUID)** + fencing version + expiry. `runtimeGeneration` do API cấp khi chấp nhận intent sang Running (đơn điệu theo workspaceUID); `runtimeUID` là Pod/VMI UID của incarnation hiện tại do operator báo — recreate runtime sau crash đổi runtimeUID và tự fence ticket/lease cũ. Cả hai đường streaming đều qua cùng lease/fence contract này.
   - *Contract cụ thể hóa theo thực nghiệm (fixture gateway nội bộ):*
     - Ticket opaque (32 byte ngẫu nhiên), TTL 60s, **một lần đúng nghĩa** — ticket bị consume ngay cả khi đã hết hạn, và ticket bị revoke không bao giờ redeem lại được (`denied` set). Concurrent redeem cùng một ticket: đúng một request nhận 303+cookie, phần còn lại 401/403 không cookie (PROVEN: gateway test suite).
     - Một session (lease) tối đa một interactive stream: **sequential replacement with fencing** — upgrade thứ hai trên cùng session fence (đóng) stream cũ thay vì bị từ chối, và **exclusive admission trong lúc handshake đang chạy** — upgrade đồng thời thứ hai nhận `409 stream_in_use` thay vì cả hai cùng sống sót qua fence. Quyết định này được chốt sau khi regression suite tái hiện được hai stream sống đồng thời (lỗi đã sửa; `go test -race` xanh trong regression suite). Redeem ticket mới = takeover: session cũ bị revoke (socket + cookie chết) trước khi cookie mới hoạt động.
     - **Origin/Host policy:** mọi request qua gateway listener phải qua Host allowlist; WS upgrade bắt buộc header `Origin` hiện diện và khớp đúng public origin + authority của request (403 `bad_origin` nếu sai); `POST /v1/launch` reject Origin lạ **và** `Sec-Fetch-Site: cross-site` (fetch metadata — Firefox không gửi Origin trên same-site POST, nên metadata là tín hiệu browser-context bắt buộc còn lại; chỉ client non-browser không header nào mới qua). Gateway chuyển `Sec-WebSocket-Origin` = Origin đã validate cho KasmVNC (upstream bắt buộc header legacy này trên upgrade). (PROVEN: gateway tests.)
    - **Security findings đã chốt thành contract (review → fix → tái kiểm chứng):**
      - *SEC-1:* `revoke` chỉ tác động ticket/lease được chỉ định — không fallback thầm lặng sang session đang sống khi ticket/leaseId cho trước không resolve (trước đây revoke theo ticket cũ giết session kế thừa).
      - *SEC-2:* control API `/v1/test/*` bắt buộc bearer token khi `POC_CONTROL_TOKEN_FILE` được cấu hình (trước đây token cấu hình bị bỏ qua khi thiếu header); khi không cấu hình thì listener loopback-only + cảnh báo lúc khởi động.
      - *SEC-3:* điều kiện upgrade = `Connection: upgrade` token **và** `Upgrade: websocket`; mọi upgrade offer không phải websocket → 400 `bad_upgrade`. Trước đây `Upgrade: h2c` + `Connection: upgrade` đi qua proxy bypass `originOK`/`admitUpgrade`.
      - *Non-blocking observations* (ghi nhận, không gate): session-id prefix lọt vào log; `POC_PUBLIC_ORIGIN`/`POC_UPSTREAM` chưa fail-loud khi cấu hình sai/scheme http; upstream `Set-Cookie` forward nguyên trạng. 
     - **Upstream boundary:** TLS tới KasmVNC verify theo CA pin của fixture (TLS ≥1.2) — wrong-CA → 502 và không request nào được relay; `Authorization` inject server-side, client `Authorization`/`Cookie` bị ghi đè/xóa; path allowlist chỉ mở asset client + `/websockify`, mọi route khác gồm `/api/*` (management surface owner-only của upstream) trả 404 trước auth — allowlist được đánh giá trên path đã normalize (`..`, `%2e`, `%2f`, `\`, `//`, `;` → 404) và path clean được forward nguyên trạng (SEC-20) (PROVEN: gateway tests, `internal/gateway` tests).
     - **Response policy trên session origin (SEC-07):** runtime do tenant kiểm soát nên response proxy qua gateway bị lọc — drop `Set-Cookie` (mọi tên, không chỉ session cookie), `Service-Worker-Allowed`, `Clear-Site-Data`, `Refresh`, `Link`; request mang `Service-Worker: script` bị 403 (không cho đăng ký service worker trên origin chia sẻ); CSP gắn cứng (`default-src 'self'`, `connect-src 'self' wss://<session-host> data:` (`data:` cho probe H.264 của client — fetch một data: URL không đi ra mạng; FX-R22), `frame-ancestors/base-uri/object-src/form-action 'none'`, chỉ nới `unsafe-inline` script/style + `wasm-unsafe-eval` + `img-src data:` + `frame-src blob:` theo đúng nhu cầu đã kiểm chứng của KasmVNC 1.5.0 client; Permissions-Policy cấp thêm `keyboard-map=(self)` để client map bố cục phím không-US), `X-Content-Type-Options: nosniff` và HSTS trên mọi response gateway.
     - **Upstream account không có quyền owner:** fixture tạo user KasmVNC bằng `kasmvncpasswd` *không* `-o` (write-only) trong entrypoint của fixture — credential do gateway inject không mở được `/api/*` phía upstream ngay cả khi lọt qua allowlist (PROVEN: upstream probe write-user `/api/get_users` → 401).
     - **Revoke đang stream:** revoke session đóng mọi socket/cancel đang mở (không chỉ chặn handshake mới); ngân sách ≤30s — đo thực tế **116ms/113ms** (chromium/firefox) trên run e2e và **9ms/12ms** trong QA độc lập; re-handshake sau revoke → 401. Session sweep 500ms + TTL session 15 phút là tham số fixture, không phải contract sản phẩm (PROVEN).
4. **Bootstrap contract (Windows, deferred):** `bootstrap-api` nội bộ (ClusterIP riêng, chỉ mTLS) expose `POST /internal/v1/bootstrap/{workspaceUID}/enroll` — first enrollment một lần/disk identity bằng one-time token + deterministic request ID, replay sau mất response trả đúng kết quả đã lưu; và `POST .../ready` — mọi boot, xác thực bằng enrolled credential, bind server-side tới incarnation hiện tại. Re-attach disk retained → token mới → re-enroll vô hiệu credential cũ.
5. **Network boundary:** default-deny; runtime chỉ egress tới cluster DNS + `bootstrap-api:443` (khi có bootstrap agent) + egress profile đã duyệt; profile InternetOnly dùng ipBlock `0.0.0.0/0` trừ Pod/Service/node/metadata CIDR, với allow riêng tới bootstrap.
   - *Evidence:* trên Cilium v1.20.2 của môi trường tham chiếu, 8/8 gate IPv4 required của profile InternetOnly PROVEN qua hai run độc lập; `egress-metadata` (169.254.169.254) không có endpoint trong môi trường tham chiếu → EXCLUDED_ENV, coverage tĩnh qua except-list; IPv6 tắt cluster-wide (`enable-ipv6=false`) → EXCLUDED_ENV, dualstack phải chứng minh lại trên môi trường có v6 (PROVEN/EXCLUDED_ENV).

## Contracts cần chứng minh (Linux trước) — trạng thái sau đối chiếu

| Contract | Trạng thái | Evidence |
|---|---|---|
| KasmVNC upstream sau authenticated reverse proxy trên RKE2 (WS, auth injection, upstream TLS validation, ticket exchange, revoke ≤30s) | **PROVEN trên image cuối** (e2e: deploy theo digest pinned, 62/62 strict-TLS Chromium+Firefox, revoke đo 116ms/113ms; teardown sạch) | e2e run trên cluster tham chiếu |
| KasmVNC 1.5.0 WebSocket-only, không HTTP tunnel fallback | **PROVEN** | upstream handshake transcript (capabilities `transports:["websocket"]`) |
| Readiness thực (X/display + endpoint streaming), không chỉ Pod Running | **PROVEN** (readiness probe + `/v1/test/ready` kiểm tra upstream probe + oracle file; ServerInit thật trong transcript) | fixture pod manifest + upstream transcript |
| Browser profile trong container non-root, sandbox hoạt động hoặc ngoại lệ ghi rõ | **PROVEN — có điều kiện node-level** — mọi securityContext pod-level fail trên node tham chiếu (EPERM unshare/uid_map), nhưng cặp node profile Localhost seccomp + AppArmor `userns` (`deploy/node-profiles/`, sha256 trong `docs/compatibility.md`) cho Chromium sandbox đầy đủ (3 renderer nested userns, Seccomp_filters≥2) — trial 11/11, rollback byte-exact cả hai lần. **Đây là prerequisite nền tảng:** phải rollout tới mọi worker chạy browser trước khi mở browser workspaces. Fallback đã đo: Firefox ESR 140.16.0esr giữ seccomp-bpf level 6 + TSYNC không cần node mutation nhưng mất lớp userns (isolation giảm) — quyết định sản phẩm | `deploy/node-profiles/` |
| Negative: truy cập runtime trực tiếp và egress ngoài allowlist bị deny trên CNI thực tế | **PROVEN** (pod→podIP:6901/18443 TIMEOUT dưới default-deny; egress IPv4 matrix 8/8 required) | kiểm chứng trên CNI thực tế |
| Sequential replacement + fencing + exclusive in-flight admission (stream policy) | **PROVEN** (27/27 `go test`, `-race -count=5` xanh; e2e counted single-stream trong run chính + reload `connections==1`/fencing trong QA độc lập) | fixture gateway (`admitUpgrade`/`endUpgrade` gen-guarded) + regression suite |
| Chi tiết cần kiểm chứng | | — |

## Hệ quả

- Hai streaming stack phải duy trì (KasmVNC + Guacamole/guacd); đổi lại mỗi workload dùng tài nguyên phù hợp và không phụ thuộc thành phần độc quyền Kasm.
- Broker/gateway là điểm bắt buộc duy nhất cho authorization; runtime không được trust truy cập trực tiếp.
- Reconnect **trong cùng một lease** (reload/refresh) dùng session cookie và fence stream cũ — sequential replacement, không cần ticket mới. Reconnect **sau incarnation replacement** (runtimeUID đổi) cần ticket mới — flow ticket 60s đã có sẵn nên không thêm cơ chế. Takeover bằng ticket mới revoke session cũ trước khi session mới hoạt động.
- Windows deferred không chặn Linux vertical slice; khi Windows mở lại, gate riêng reuse cùng lease/fence contract.

## Alternatives đã xem xét

- Mọi desktop là KubeVirt VM (kể cả Linux): isolation mạnh hơn nhưng tốn RAM/disk và boot chậm — giữ làm LinuxVM backend sau MVP.
- Port KasmVNC thành Windows server: phải duy trì display/input/encoding stack trên Windows — từ chối.
- guacd nối KasmVNC như VNC/RFB: không được giả định tương thích vì protocol KasmVNC khác RFB truyền thống.

## Nguồn

- [KasmVNC](https://github.com/kasmtech/KasmVNC), [KasmVNC server-side auth](https://www.kasmweb.com/kasmvnc/docs/latest/serverside.html)
- [Guacamole architecture](https://guacamole.apache.org/doc/gug/guacamole-architecture.html), [Guacamole custom auth](https://guacamole.apache.org/doc/gug/custom-auth.html)
- Design: [docs/architecture.md](../architecture.md)
- Evidence: đợt kiểm chứng 2026-09-30.
