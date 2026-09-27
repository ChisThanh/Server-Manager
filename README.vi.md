<div align="center">

<img src="images/logo.svg" alt="Server Manager logo" width="120" height="120" />

# Server Manager

**Quản lý server Linux từ một ứng dụng desktop — chỉ qua SSH, không cài gì lên server.**

Giám sát, log, terminal, file, Docker, triển khai, Nginx/Caddy + SSL, database, sao lưu, tường lửa & fail2ban, cảnh báo, chạy lệnh trên nhiều server và nhật ký kiểm toán — gói gọn trong một app nhanh, mượt.

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Status: alpha](https://img.shields.io/badge/status-alpha-orange)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![React](https://img.shields.io/badge/React-18-61DAFB?logo=react&logoColor=black)
![Wails](https://img.shields.io/badge/Wails-v3-DF0000)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

[English](README.md) · Tiếng Việt

![Tổng quan server](images/index.png)

</div>

> [!WARNING]
> **Phần mềm đang ở giai đoạn alpha, được viết phần lớn cùng AI ("vibe-code").** Có unit test và integration test chạy trên server Linux thật (trong Docker), nhưng **chưa** được kiểm chứng kỹ trên production với nhiều bản phân phối và cấu hình khác nhau. App chạy lệnh bằng quyền root trên server của bạn — **hãy thử trên server staging trước**, đọc kỹ thao tác trước khi xác nhận, và [báo lỗi](../../issues) khi gặp vấn đề. Hiện tại, đóng góp giá trị nhất là thêm test, review code và xử lý các trường hợp biên.

## Vì sao có dự án này

Phần lớn các dashboard quản lý server đòi cài thêm thứ gì đó lên server: agent, web panel, hay container có quyền vào Docker socket. Mỗi thứ lại là một thứ phải cập nhật, bảo mật và mở cổng.

Server Manager đi ngược lại: **chỉ cần SSH**. Mọi thao tác đi qua `sshd` có sẵn (exec, PTY, SFTP) và dùng công cụ sẵn có của hệ điều hành (`/proc`, `systemctl`, `journalctl`, `docker`, `nginx`, `ufw`, `psql`…). Tắt app đi, server vẫn y nguyên như cũ.

| | Server Manager | Cockpit / Webmin | Portainer | SSH client thường |
|---|:-:|:-:|:-:|:-:|
| Không cài gì lên server | ✅ | ❌ (web panel trên server) | ❌ (agent / container) | ✅ |
| Giám sát + cảnh báo | ✅ | một phần | ❌ | ❌ |
| Docker, Compose, triển khai | ✅ | plugin | ✅ | ❌ |
| Site Nginx/Caddy + Let's Encrypt | ✅ | một phần | ❌ | ❌ |
| Tường lửa, fail2ban, bảo mật SSH | ✅ | một phần | ❌ | ❌ |
| Nhiều server cùng lúc + nhật ký kiểm toán | ✅ | một phần | một phần | ❌ |
| Không mở thêm cổng nào ngoài SSH | ✅ | ❌ | ❌ | ✅ |

## Tính năng

| Nhóm | Nội dung |
|---|---|
| **Tổng quan & giám sát** | Tình trạng sức khoẻ ("7/7 kiểm tra ổn"), CPU từng nhân, iowait/steal, load, RAM/swap, ổ đĩa (dung lượng, inode, IO, độ trễ), mạng (băng thông, lỗi), top tiến trình. Lịch sử 5 phút → 30 ngày, kéo để phóng to biểu đồ. |
| **Health check & cảnh báo** | Quy tắc trên mọi chỉ số với ngưỡng và thời gian kéo dài, phạm vi theo môi trường/nhóm/tag/server. Chờ → cảnh báo → xác nhận → hết, có chống nhiễu và giới hạn tần suất. Kênh gửi: Telegram, Slack, Discord, email (SMTP), webhook có ký, PagerDuty, thông báo desktop. |
| **Logs** | journald, syslog, auth, kernel, Nginx, Docker, file bất kỳ — lọc mức độ/thời gian, tìm regex, xem trực tiếp, vẫn mượt với hơn 10.000 dòng, tải về. |
| **Terminal** | Nhiều tab, tìm kiếm, console thẳng vào container và database, ghi & phát lại phiên, gõ tiếng Việt (Telex/VNI) chuẩn. |
| **File** | Duyệt, upload/download, đổi tên, chmod/chown, sudo, trình soạn Monaco. *Lưu & nạp lại* kiểm tra cấu hình Nginx/Caddy/Apache/sshd/systemd trước, lỗi thì tự khôi phục. |
| **Dịch vụ & tiến trình** | systemd start/stop/restart/enable, chi tiết (PID, CPU, RAM, số lần restart, phụ thuộc), tự khởi động lại, log; danh sách tiến trình, kill. |
| **Docker** | Container (thống kê, log, exec, inspect, env, volume, network, tạo lại), image, volume, network; Compose: sửa → kiểm tra → triển khai → rollback. |
| **Triển khai** | App từ git hoặc image, biến môi trường và secret: build → test → restart → health check, lịch sử, tự rollback khi health check lỗi. |
| **Web & SSL** | Reverse proxy Nginx/Caddy từ form (chuyển HTTPS, HSTS, WebSocket, header, giới hạn tốc độ, basic auth), kiểm tra cấu hình trước khi áp dụng. Let's Encrypt cấp/gia hạn/thu hồi, theo dõi hạn, cài chứng chỉ riêng. |
| **Bảo mật** | Điểm bảo mật, cấu hình sshd an toàn, authorized_keys, tường lửa (ufw / firewalld / iptables / xem nftables), cổng đang mở, đăng nhập thất bại, lịch sử sudo, thay đổi tài khoản. |
| **Chặn / cho phép IP** | Xem IP đang dò mật khẩu SSH (gộp theo dải /24), chặn nhiều IP hoặc cả dải bằng một cú bấm (luật tường lửa vĩnh viễn hoặc ban tạm bằng fail2ban), danh sách cho phép, "chỉ cho SSH từ IP được phép", cài & cấu hình fail2ban một chạm (ban tăng dần, jail recidive). Không bao giờ chặn IP của chính bạn. |
| **Người dùng Linux** | Tạo/xoá, khoá/mở khoá, đặt mật khẩu, nhóm, quyền sudo. |
| **Database** | PostgreSQL, MySQL/MariaDB, Redis (trên máy chủ hoặc Docker): trạng thái, kết nối, dung lượng, truy vấn đang chạy và chậm, chạy truy vấn (mặc định chỉ đọc). |
| **Sao lưu** | File, PostgreSQL, MySQL, Docker volume, Redis → thư mục trên máy, trên server hoặc S3 (AWS, Backblaze B2, MinIO, Cloudflare R2…). Mã hoá age, lịch cron, giữ bản theo số lượng/ngày, kiểm tra toàn vẹn, khôi phục. |
| **Nhiều server & Command Center** | Môi trường, tag, nhóm, vùng, nhà cung cấp; chạy lệnh hoặc lệnh mẫu trên nhiều server: xác nhận, phát hiện lệnh nguy hiểm, timeout, giới hạn chạy song song, kết quả từng server. |
| **Kiểm toán & quyền hạn** | Ghi lại mọi thay đổi (ai, lúc nào, server nào, làm gì, kết quả) trong chuỗi băm SHA-256, xuất CSV. Vai trò theo từng server (Admin / Operator / Developer / Chỉ xem) kiểm tra ở backend; server production bắt buộc gõ tên server cho thao tác nguy hiểm. |
| **Chạy nền** | Đóng cửa sổ, app vẫn giám sát, gửi cảnh báo và sao lưu theo lịch từ thanh menu / khay hệ thống. |
| **10 ngôn ngữ** | Tiếng Việt, English, 简体中文, 日本語, 한국어, Français, Deutsch, Español, Português, Русский. |

<div align="center">

![Màn hình chào](images/home.png)

</div>

## Bảo mật

- **Mật khẩu, passphrase, token, secret nằm trong Keychain của hệ điều hành** (macOS Keychain, Windows Credential Manager, Secret Service trên Linux). SQLite chỉ lưu cấu hình không bí mật, số liệu và nhật ký kiểm toán.
- **Kiểm tra host key**: hỏi khi gặp server lạ, cảnh báo khi key thay đổi.
- **Chống chèn lệnh ngay từ thiết kế**: mọi giá trị ghép vào lệnh đều được kiểm tra và đặt trong dấu nháy đơn; secret truyền qua stdin, không nằm trên dòng lệnh (không lộ ra `ps`) và được che trong log triển khai.
- **Rào chắn an toàn**: thao tác có thể khoá bạn khỏi server (luật tường lửa, đổi sshd, xoá khoá cuối cùng, chặn IP của chính bạn) bị phát hiện và từ chối hoặc phải xác nhận rõ ràng; sửa cấu hình được kiểm tra và tự khôi phục khi lỗi.

Phát hiện lỗ hổng? Vui lòng làm theo [SECURITY.md](SECURITY.md) thay vì mở issue công khai.

## Công nghệ

| Lớp | Công nghệ |
|---|---|
| Vỏ desktop | [Wails v3](https://v3.wails.io) (backend Go + webview gốc của hệ điều hành, không dùng Electron) |
| Backend | Go 1.26 · `golang.org/x/crypto/ssh` · `pkg/sftp` · SQLite ([modernc](https://pkg.go.dev/modernc.org/sqlite), thuần Go) · [go-keyring](https://github.com/zalando/go-keyring) · [age](https://age-encryption.org) · [minio-go](https://github.com/minio/minio-go) (S3) · robfig/cron |
| Frontend | React 18 · TypeScript · Vite · [zustand](https://github.com/pmndrs/zustand) · [Monaco Editor](https://microsoft.github.io/monaco-editor/) · [xterm.js](https://xtermjs.org) · [uPlot](https://github.com/leeoniya/uPlot) · icon [lucide](https://lucide.dev) |
| Trên server | Không có gì mới — chỉ `sshd` và công cụ có sẵn của hệ điều hành |
| Kiểm thử | Unit test Go + integration test trên server Debian, Alpine và AlmaLinux (firewalld) thật trong Docker (`testenv/`) |

## Bắt đầu

Chưa có bản build sẵn — hãy build từ mã nguồn (mất vài phút).

**Yêu cầu:** Go 1.26+, Node.js 20+, [Wails v3 CLI](https://v3.wails.io/getting-started/installation/) và các yêu cầu nền tảng của Wails (Xcode command line tools trên macOS, WebKitGTK trên Linux, WebView2 trên Windows).

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@latest
git clone https://github.com/<your-github-username>/server-manager.git
cd server-manager

wails3 dev        # chạy chế độ phát triển (hot reload)
wails3 build      # build ra bin/
wails3 package    # đóng gói (.app / installer)
```

Sau đó bấm **Thêm server**, nhập `user@host[:port]` và mật khẩu, key hoặc SSH agent. Vậy là xong — không có gì được cài lên server.

**Server hỗ trợ:** Linux có `sshd` — phát triển và kiểm thử trên Debian/Ubuntu, Alpine và họ AlmaLinux/RHEL. Tài khoản root hoặc có `sudo` mở khoá các tính năng quản trị; tài khoản thường vẫn xem được giám sát, log, file và terminal.

**Máy desktop hỗ trợ:** phát triển trên macOS. Wails hỗ trợ build cho Linux và Windows nhưng hai nền tảng này mới được thử rất ít — rất mong nhận báo cáo và bản sửa.

## Giới hạn hiện tại

- **Lịch sử số liệu và cảnh báo chỉ có khi app đang chạy** (chế độ chạy nền giữ app chạy ở khay hệ thống). Giám sát nền cần lưu mật khẩu/passphrase vào Keychain hoặc dùng key/agent.
- **Vai trò là rào chắn cục bộ** trên máy dùng app, chưa phải phân quyền cho cả nhóm. RBAC/SSO dùng chung cần một control-plane server — xem lộ trình.
- Traefik mới được phát hiện, chưa chỉnh sửa được (Traefik cấu hình bằng label Docker).
- Bản thân Wails v3 vẫn đang beta.

## Lộ trình

- [ ] Bản build sẵn có ký số cho macOS, Windows, Linux + tự cập nhật
- [ ] Kiểm thử rộng hơn: Ubuntu LTS, Rocky/Alma, Fedora, Arch, openSUSE; desktop Windows và Linux
- [ ] Control plane tự host (tuỳ chọn) cho nhóm: dùng chung server, RBAC, SSO
- [ ] Sửa nftables trực tiếp, tích hợp CrowdSec, hỗ trợ Traefik
- [ ] Thêm loại database và nơi lưu bản sao lưu
- [ ] Plugin API cho panel tuỳ biến

Có ý tưởng? [Mở một đề xuất tính năng](../../issues/new/choose).

## Đóng góp

Dự án mở mã nguồn để ngày càng tốt hơn nhờ có thêm nhiều người cùng xem. Mọi đóng góp đều được hoan nghênh: báo lỗi, viết test, review, dịch thuật, tính năng mới — bắt đầu từ [CONTRIBUTING.md](CONTRIBUTING.md). Các issue gắn nhãn `good first issue` là điểm khởi đầu tốt.

Tài liệu cho lập trình viên [docs/DEV_GUIDE.md](docs/DEV_GUIDE.md) giải thích kiến trúc và quy ước (cấu trúc một module, cách chạy lệnh an toàn, i18n, quy tắc hiệu năng).

## Giấy phép

[MIT](LICENSE) — dùng tự do cho cá nhân và thương mại.

Nếu Server Manager giúp bạn tiết kiệm thời gian, một ⭐ trên GitHub sẽ giúp nhiều người khác biết đến dự án.
