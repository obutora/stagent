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
	// Digest titles (%d: count) and the line closing a long digest body.
	PhraseDigest
	PhraseDigestTurnComplete
	PhraseDigestNeedsApproval
	PhraseDigestWaitingInput
	PhraseDigestExited
	PhraseDigestMore
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
