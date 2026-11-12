// oshirase-notifier posts one notification through the UserNotifications
// framework and exits. It lives inside Oshirase.app because macOS grants
// notification permission to an app bundle, not to a bare binary; the daemon
// runs the executable inside the bundle directly.
//
//   oshirase-notifier post <title> <body> <subtitle> <urgency>
//   oshirase-notifier authorize
//
// Exit status 0 means macOS accepted the notification and alerts are on for
// this app. Anything else is a refusal, explained on stderr, so the daemon
// can answer the caller with an error instead of a silent success.
//
// What it cannot see: Focus. A notification accepted here may still be held
// back by a Focus that does not allow this app.

import Foundation
import UserNotifications

@main
struct Notifier {
    static func main() async {
        let arguments = Array(CommandLine.arguments.dropFirst())
        do {
            switch arguments.first {
            case "post" where arguments.count == 5:
                try await post(
                    title: arguments[1], body: arguments[2],
                    subtitle: arguments[3], urgency: arguments[4])
            case "authorize" where arguments.count == 1:
                try await authorize(prompt: true)
                print("oshirase may show notifications")
            case nil:
                // macOS launches the app with no arguments when someone clicks
                // one of its notifications. There is nothing to open.
                break
            default:
                fail(2, "usage: oshirase-notifier post <title> <body> <subtitle> <urgency> | authorize")
            }
        } catch let refusal as Refusal {
            fail(1, refusal.message)
        } catch {
            fail(1, error.localizedDescription)
        }
        exit(0)
    }

    static func post(title: String, body: String, subtitle: String, urgency: String) async throws {
        // Never prompt from here: the daemon kills this process after ten
        // seconds, long before anyone answers a prompt, and the notification
        // would be lost with nothing said about why.
        try await authorize(prompt: false)

        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.subtitle = subtitle
        switch urgency {
        case "critical":
            // Time Sensitive and critical alerts need entitlements Apple grants
            // only through a provisioning profile; a sound is what is left.
            content.sound = .default
            content.relevanceScore = 1
        case "low":
            content.relevanceScore = 0
        default:
            content.relevanceScore = 0.5
        }

        let request = UNNotificationRequest(
            identifier: UUID().uuidString, content: content, trigger: nil)
        try await UNUserNotificationCenter.current().add(request)
    }

    // authorize checks that the person has not taken permission back or
    // turned the alerts off, which macOS would otherwise apply silently. With
    // prompt, it first asks for permission if nobody has been asked yet.
    static func authorize(prompt: Bool) async throws {
        let center = UNUserNotificationCenter.current()
        var settings = await center.notificationSettings()

        if settings.authorizationStatus == .notDetermined {
            guard prompt else {
                throw Refusal("oshirase has not been allowed to show notifications yet; run install-macos.sh, or open -W -a ~/Applications/Oshirase.app --args authorize")
            }
            let granted = try await center.requestAuthorization(options: [.alert, .sound])
            if !granted {
                throw Refusal("notifications for oshirase were declined; allow them in System Settings → Notifications → oshirase")
            }
            settings = await center.notificationSettings()
        }

        switch settings.authorizationStatus {
        case .authorized, .provisional:
            break
        case .denied:
            throw Refusal("notifications for oshirase are turned off in System Settings → Notifications → oshirase")
        default:
            throw Refusal("macOS has not allowed oshirase to show notifications (status \(settings.authorizationStatus.rawValue))")
        }

        if settings.alertStyle == .none {
            throw Refusal("oshirase's alert style is None, so notifications go only to Notification Center; choose Banners or Alerts in System Settings → Notifications → oshirase")
        }
    }

    static func fail(_ status: Int32, _ message: String) -> Never {
        FileHandle.standardError.write(Data((message + "\n").utf8))
        exit(status)
    }
}

struct Refusal: Error {
    let message: String
    init(_ message: String) { self.message = message }
}
