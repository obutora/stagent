package notify

import (
	"fmt"

	"github.com/obutora/stagent/internal/wire"
)

// Phrase is one of the fixed texts stagent writes into notifications.
// Program output never becomes a phrase.
type Phrase int

const (
	PhraseWaitingInput  Phrase = iota
	PhraseTurnComplete         //
	PhraseNeedsApproval        // followed by ": <tool>" when known
	PhraseExited               //
	PhraseExitedCode           // %d: exit code
	PhraseSessionLost          //
	PhraseTerminal             // push body of a program's own notification
	PhraseBell                 // in-app body of a bare BEL
	PhraseTest                 // notify.test
	PhraseOpen                 // the chat destination's button to the open page
	PhraseResolved             // title suffix of a chat message marked resolved
	// Digest titles (%d: count) and the line closing a long digest body.
	PhraseDigest
	PhraseDigestTurnComplete
	PhraseDigestNeedsApproval
	PhraseDigestWaitingInput
	PhraseDigestExited
	PhraseDigestMore
	// The bodies of a schedule's notifications (reason schedule) and the
	// digest title of several.
	PhraseScheduleSucceeded
	PhraseScheduleFixed
	PhraseScheduleFailed
	PhraseScheduleFailedCode   // %d: exit code
	PhraseScheduleTimedOut     //
	PhraseScheduleDenied       // %d: refused actions
	PhraseScheduleUndetermined //
	PhraseScheduleSkipped      //
	PhraseScheduleAutoStopped  // %d: failures in a row
	PhraseScheduleGUILogin     // %s: the agent
	PhraseDigestSchedule       // %d: count
	// The bodies of a limit window's notifications (reason usage_limit),
	// the window in their titles and the digest title of several.
	PhraseLimitUsed           // %d: the share passed (80, 100), %s: the reset time
	PhraseLimitReset          //
	PhraseWindowFiveHour      //
	PhraseWindowSevenDay      //
	PhraseWindowSevenDayModel // %s: the model ("Opus")
	PhraseWindowDays          // %d: days
	PhraseWindowHours         // %d: hours
	PhraseWindowMinutes       // %d: minutes
	PhraseDigestUsageLimit    // %d: count
	// The body of a question with several questions (%s: the first, %d:
	// how many); one question's body is its text alone.
	PhraseQuestions
	phraseCount
)

var phrases = map[string][phraseCount]string{
	wire.LangEn: {
		PhraseWaitingInput:        "Waiting for input",
		PhraseTurnComplete:        "Turn complete",
		PhraseNeedsApproval:       "Needs approval",
		PhraseExited:              "Exited",
		PhraseExitedCode:          "Exited with code %d",
		PhraseSessionLost:         "Session lost (its holder stopped)",
		PhraseTerminal:            "Terminal notification",
		PhraseBell:                "Bell",
		PhraseTest:                "Test notification from stagent",
		PhraseDigest:              "%d agent notifications",
		PhraseDigestTurnComplete:  "%d agents finished their turn",
		PhraseDigestNeedsApproval: "%d approvals requested",
		PhraseDigestWaitingInput:  "%d agents waiting for input",
		PhraseDigestExited:        "%d sessions exited",
		PhraseDigestMore:          "…and %d more",
		PhraseOpen:                "Open in SSH Term",
		PhraseResolved:            " (resolved)",

		PhraseScheduleSucceeded:    "Succeeded",
		PhraseScheduleFixed:        "Succeeded again: fixed",
		PhraseScheduleFailed:       "Failed",
		PhraseScheduleFailedCode:   "Failed (exit code %d)",
		PhraseScheduleTimedOut:     "Stopped at its time limit",
		PhraseScheduleDenied:       "%d actions were refused",
		PhraseScheduleUndetermined: "Could not tell the result",
		PhraseScheduleSkipped:      "Skipped: the previous run was still running",
		PhraseScheduleAutoStopped:  "Stopped after %d failures in a row",
		PhraseScheduleGUILogin:     "%s could not authenticate: nobody is logged in to the GUI",
		PhraseDigestSchedule:       "%d schedule notifications",

		PhraseLimitUsed:           "%d%% used · resets at %s",
		PhraseLimitReset:          "Reset",
		PhraseWindowFiveHour:      "5h",
		PhraseWindowSevenDay:      "Week",
		PhraseWindowSevenDayModel: "Week (%s)",
		PhraseWindowDays:          "%dd",
		PhraseWindowHours:         "%dh",
		PhraseWindowMinutes:       "%dm",
		PhraseDigestUsageLimit:    "%d limit window notifications",

		PhraseQuestions: "%s (%d questions)",
	},
	wire.LangJa: {
		PhraseWaitingInput:        "入力待ち",
		PhraseTurnComplete:        "ターン完了",
		PhraseNeedsApproval:       "承認待ち",
		PhraseExited:              "終了しました",
		PhraseExitedCode:          "終了コード %d で終了しました",
		PhraseSessionLost:         "セッションが失われました（holder が止まりました）",
		PhraseTerminal:            "端末からの通知",
		PhraseBell:                "ベル",
		PhraseTest:                "stagent からのテスト通知",
		PhraseDigest:              "agent の通知 %d 件",
		PhraseDigestTurnComplete:  "%d 件の agent がターンを終えました",
		PhraseDigestNeedsApproval: "承認待ち %d 件",
		PhraseDigestWaitingInput:  "入力待ちの agent %d 件",
		PhraseDigestExited:        "%d 件のセッションが終了しました",
		PhraseDigestMore:          "…ほか %d 件",
		PhraseOpen:                "SSH Term で開く",
		PhraseResolved:            "（解決済み）",

		PhraseScheduleSucceeded:    "成功しました",
		PhraseScheduleFixed:        "直りました（成功しました）",
		PhraseScheduleFailed:       "失敗しました",
		PhraseScheduleFailedCode:   "失敗しました（終了コード %d）",
		PhraseScheduleTimedOut:     "時間切れで止めました",
		PhraseScheduleDenied:       "操作を %d 件断られました",
		PhraseScheduleUndetermined: "結果を判定できませんでした",
		PhraseScheduleSkipped:      "前の実行が終わっていないので飛ばしました",
		PhraseScheduleAutoStopped:  "%d 回続けて失敗したので止めました",
		PhraseScheduleGUILogin:     "GUI にログインしていないので %s が認証できませんでした",
		PhraseDigestSchedule:       "定期実行の通知 %d 件",

		PhraseLimitUsed:           "%d%% を使いました · %s にリセット",
		PhraseLimitReset:          "リセットされました",
		PhraseWindowFiveHour:      "5時間",
		PhraseWindowSevenDay:      "週",
		PhraseWindowSevenDayModel: "週（%s）",
		PhraseWindowDays:          "%d日",
		PhraseWindowHours:         "%d時間",
		PhraseWindowMinutes:       "%d分",
		PhraseDigestUsageLimit:    "上限の枠の通知 %d 件",

		PhraseQuestions: "%s（全 %d 問）",
	},
	wire.LangKo: {
		PhraseWaitingInput:        "입력 대기",
		PhraseTurnComplete:        "턴 완료",
		PhraseNeedsApproval:       "승인 필요",
		PhraseExited:              "종료됨",
		PhraseExitedCode:          "종료 코드 %d(으)로 종료됨",
		PhraseSessionLost:         "세션을 잃었습니다 (holder가 멈춤)",
		PhraseTerminal:            "터미널 알림",
		PhraseBell:                "벨",
		PhraseTest:                "stagent 테스트 알림",
		PhraseDigest:              "에이전트 알림 %d개",
		PhraseDigestTurnComplete:  "에이전트 %d개가 턴을 마쳤습니다",
		PhraseDigestNeedsApproval: "승인 요청 %d개",
		PhraseDigestWaitingInput:  "입력 대기 중인 에이전트 %d개",
		PhraseDigestExited:        "세션 %d개가 종료되었습니다",
		PhraseDigestMore:          "…외 %d개",
		PhraseOpen:                "SSH Term에서 열기",
		PhraseResolved:            " (해결됨)",

		PhraseScheduleSucceeded:    "성공했습니다",
		PhraseScheduleFixed:        "복구되었습니다(성공)",
		PhraseScheduleFailed:       "실패했습니다",
		PhraseScheduleFailedCode:   "실패했습니다(종료 코드 %d)",
		PhraseScheduleTimedOut:     "시간 제한으로 중지했습니다",
		PhraseScheduleDenied:       "작업 %d개가 거부되었습니다",
		PhraseScheduleUndetermined: "결과를 판정할 수 없습니다",
		PhraseScheduleSkipped:      "이전 실행이 끝나지 않아 건너뛰었습니다",
		PhraseScheduleAutoStopped:  "%d번 연속 실패하여 중지했습니다",
		PhraseScheduleGUILogin:     "GUI에 로그인하지 않아 %s이(가) 인증하지 못했습니다",
		PhraseDigestSchedule:       "예약 실행 알림 %d개",

		PhraseLimitUsed:           "%d%% 사용 · %s에 초기화",
		PhraseLimitReset:          "초기화되었습니다",
		PhraseWindowFiveHour:      "5시간",
		PhraseWindowSevenDay:      "주간",
		PhraseWindowSevenDayModel: "주간(%s)",
		PhraseWindowDays:          "%d일",
		PhraseWindowHours:         "%d시간",
		PhraseWindowMinutes:       "%d분",
		PhraseDigestUsageLimit:    "사용 한도 알림 %d개",

		PhraseQuestions: "%s (질문 %d개)",
	},
	wire.LangZh: {
		PhraseWaitingInput:        "等待输入",
		PhraseTurnComplete:        "本轮完成",
		PhraseNeedsApproval:       "需要批准",
		PhraseExited:              "已退出",
		PhraseExitedCode:          "已退出，退出码 %d",
		PhraseSessionLost:         "会话已丢失（holder 已停止）",
		PhraseTerminal:            "终端通知",
		PhraseBell:                "响铃",
		PhraseTest:                "来自 stagent 的测试通知",
		PhraseDigest:              "%d 条智能体通知",
		PhraseDigestTurnComplete:  "%d 个智能体已完成本轮",
		PhraseDigestNeedsApproval: "%d 个批准请求",
		PhraseDigestWaitingInput:  "%d 个智能体等待输入",
		PhraseDigestExited:        "%d 个会话已退出",
		PhraseDigestMore:          "…另有 %d 条",
		PhraseOpen:                "在 SSH Term 中打开",
		PhraseResolved:            "（已解决）",

		PhraseScheduleSucceeded:    "已成功",
		PhraseScheduleFixed:        "已恢复（成功）",
		PhraseScheduleFailed:       "已失败",
		PhraseScheduleFailedCode:   "已失败（退出码 %d）",
		PhraseScheduleTimedOut:     "已因超时停止",
		PhraseScheduleDenied:       "%d 个操作被拒绝",
		PhraseScheduleUndetermined: "无法判定结果",
		PhraseScheduleSkipped:      "上一次运行尚未结束，已跳过",
		PhraseScheduleAutoStopped:  "连续失败 %d 次，已停止",
		PhraseScheduleGUILogin:     "未登录 GUI，%s 无法认证",
		PhraseDigestSchedule:       "%d 条定时运行通知",

		PhraseLimitUsed:           "已使用 %d%% · %s 重置",
		PhraseLimitReset:          "已重置",
		PhraseWindowFiveHour:      "5 小时",
		PhraseWindowSevenDay:      "每周",
		PhraseWindowSevenDayModel: "每周（%s）",
		PhraseWindowDays:          "%d 天",
		PhraseWindowHours:         "%d 小时",
		PhraseWindowMinutes:       "%d 分钟",
		PhraseDigestUsageLimit:    "%d 条用量上限通知",

		PhraseQuestions: "%s（共 %d 个问题）",
	},
}

// Text is phrase p in lang (English for an unknown language), formatted
// with args.
func Text(lang string, p Phrase, args ...any) string {
	t, ok := phrases[lang]
	if !ok {
		t = phrases[wire.LangEn]
	}
	if len(args) == 0 {
		return t[p]
	}
	return fmt.Sprintf(t[p], args...)
}
