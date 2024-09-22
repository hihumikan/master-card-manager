package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
)

// KeyStatus は各キーの貸し出し状況を表します。
type KeyStatus struct {
	KeyNumber  string
	Borrower   string
	BorrowedAt time.Time
}

// Bot はSlackボットの構造体です。
type Bot struct {
	api         *slack.Client
	keyStatuses map[string]*KeyStatus
	mutex       sync.Mutex
	channelID   string
	botUserID   string
}

// NewBot は新しいBotインスタンスを作成します。
func NewBot(token, channelName string) (*Bot, error) {
	api := slack.New(token)

	// チャンネル名からチャンネルIDを取得
	channelID, err := getChannelID(api, channelName)
	if err != nil {
		return nil, err
	}

	// チャンネルに参加
	err = joinChannel(api, channelID)
	if err != nil {
		// チャンネルに既に参加している場合のエラーを無視
		if !strings.Contains(err.Error(), "already_in_channel") {
			return nil, fmt.Errorf("チャンネルに参加できませんでした: %v", err)
		}
	}

	// ボットのユーザーIDを取得
	authTest, err := api.AuthTest()
	if err != nil {
		return nil, fmt.Errorf("AuthTest failed: %v", err)
	}

	return &Bot{
		api:         api,
		keyStatuses: make(map[string]*KeyStatus),
		channelID:   channelID,
		botUserID:   authTest.UserID,
	}, nil
}

// getChannelID はチャンネル名からチャンネルIDを取得します。
func getChannelID(api *slack.Client, channelName string) (string, error) {
	params := slack.GetConversationsParameters{
		Limit: 1000,
		Types: []string{"public_channel", "private_channel"}, // パブリックとプライベートを含める
	}
	channels, _, err := api.GetConversations(&params)
	if err != nil {
		return "", err
	}

	for _, ch := range channels {
		if ch.Name == channelName {
			return ch.ID, nil
		}
	}
	return "", fmt.Errorf("channel %s not found", channelName)
}

// joinChannel は指定されたチャンネルに参加します。
func joinChannel(api *slack.Client, channelID string) error {
	_, _, _, err := api.JoinConversation(channelID)
	return err
}

// listChannels はボットがアクセスできるチャンネルの一覧をログに出力します。
func listChannels(api *slack.Client) {
	params := slack.GetConversationsParameters{
		Limit: 1000,
		Types: []string{"public_channel", "private_channel"},
	}
	channels, _, err := api.GetConversations(&params)
	if err != nil {
		log.Fatalf("チャンネルリストの取得に失敗しました: %v", err)
	}

	log.Println("アクセス可能なチャンネル一覧:")
	for _, ch := range channels {
		log.Printf("- %s (%s)\n", ch.Name, ch.ID)
	}
}

// Run はボットを起動します。
func (b *Bot) Run() {
	for {
		rtm := b.api.NewRTM()
		go rtm.ManageConnection()

		// 定期的に過去2日以上返却されていないキーをチェック
		go b.overdueChecker()

		for msg := range rtm.IncomingEvents {
			switch ev := msg.Data.(type) {
			case *slack.MessageEvent:
				log.Printf("Message Event received: ChannelID=%s, UserID=%s, Text=%s\n", ev.Channel, ev.User, ev.Text)
				b.handleMessage(ev, rtm)
			case *slack.InvalidAuthEvent:
				log.Fatalf("Invalid credentials")
			case *slack.RTMError:
				log.Printf("RTM error: %s\n", ev.Error())
			case *slack.ConnectionErrorEvent:
				log.Printf("Connection error: %s\n", ev.Error())
			default:
				// 他のイベントは無視
			}
		}

		log.Println("RTM connection closed. Reconnecting in 5 seconds...")
		time.Sleep(5 * time.Second) // 遅延後に再接続
	}
}

func (b *Bot) handleMessage(ev *slack.MessageEvent, rtm *slack.RTM) {
	text := ev.Text
	user := ev.User

	log.Printf("Handling message: %s from user: %s in channel: %s\n", text, user, ev.Channel)

	// 対象のチャンネル以外のメッセージは無視
	if ev.Channel != b.channelID {
		log.Println("Message is not in the target channel. Ignoring.")
		return
	}

	// メッセージ内容を解析
	borrowRegex := regexp.MustCompile(`(?i)(\d{2})\s*番?\s*(借ります|借りる|借りたい)`)
	returnRegex := regexp.MustCompile(`(?i)(\d{2})\s*番?\s*(返します|返す|返却します)`)

	if matches := borrowRegex.FindStringSubmatch(text); len(matches) >= 3 {
		keyNum := matches[1]
		log.Printf("Detected borrow command for key: %s\n", keyNum)
		b.borrowKey(keyNum, user, rtm)
		return
	}

	if matches := returnRegex.FindStringSubmatch(text); len(matches) >= 3 {
		keyNum := matches[1]
		log.Printf("Detected return command for key: %s\n", keyNum)
		b.returnKey(keyNum, user, rtm)
		return
	}

	// ボットがメンションされた場合、状態を報告
	mention := fmt.Sprintf("<@%s>", b.botUserID)
	if strings.Contains(text, mention) {
		log.Println("Bot was mentioned. Reporting status.")
		b.reportStatus(rtm, ev.Channel)
		return
	}

	log.Println("No actionable command detected in the message.")
}

// borrowKey は指定されたキーを借りる処理を行います。
func (b *Bot) borrowKey(keyNum, user string, rtm *slack.RTM) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if keyNum != "13" && keyNum != "14" && keyNum != "15" {
		rtm.SendMessage(rtm.NewOutgoingMessage(fmt.Sprintf("カード番号が無効です: %s", keyNum), b.channelID))
		return
	}

	if status, exists := b.keyStatuses[keyNum]; exists {
		// ユーザー名を取得
		userName, err := b.getUserName(status.Borrower)
		if err != nil {
			userName = status.Borrower
		}
		rtm.SendMessage(rtm.NewOutgoingMessage(fmt.Sprintf("カード番号%sは既に%sさんが借りています。", keyNum, userName), b.channelID))
		return
	}

	b.keyStatuses[keyNum] = &KeyStatus{
		KeyNumber:  keyNum,
		Borrower:   user,
		BorrowedAt: time.Now(),
	}

	userName, err := b.getUserName(user)
	if err != nil {
		userName = user
	}
	rtm.SendMessage(rtm.NewOutgoingMessage(fmt.Sprintf("カード番号%sを%sさんが借りました。", keyNum, userName), b.channelID))
}

// returnKey は指定されたキーを返却する処理を行います。
func (b *Bot) returnKey(keyNum, user string, rtm *slack.RTM) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	status, exists := b.keyStatuses[keyNum]
	if !exists {
		rtm.SendMessage(rtm.NewOutgoingMessage(fmt.Sprintf("カード番号%sは現在貸し出されていません。", keyNum), b.channelID))
		return
	}

	if status.Borrower != user {
		borrowerName, err := b.getUserName(status.Borrower)
		if err != nil {
			borrowerName = status.Borrower
		}
		rtm.SendMessage(rtm.NewOutgoingMessage(fmt.Sprintf("カード番号%sは%sさんが借りています。あなたは借りていません。", keyNum, borrowerName), b.channelID))
		return
	}

	delete(b.keyStatuses, keyNum)
	rtm.SendMessage(rtm.NewOutgoingMessage(fmt.Sprintf("カード番号%sが返却されました。", keyNum), b.channelID))
}

// reportStatus は現在のキーの貸し出し状況を報告します。
func (b *Bot) reportStatus(rtm *slack.RTM, channelID string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if len(b.keyStatuses) == 0 {
		rtm.SendMessage(rtm.NewOutgoingMessage("現在、貸し出されているマスターキーはありません。", channelID))
		return
	}

	var report strings.Builder
	report.WriteString("現在のマスターキーの状態:\n")
	for _, status := range b.keyStatuses {
		userName, err := b.getUserName(status.Borrower)
		if err != nil {
			userName = status.Borrower
		}
		report.WriteString(fmt.Sprintf("カード番号%s: %sさんが借りています。借りた日: %s\n",
			status.KeyNumber, userName, status.BorrowedAt.Format("2006-01-02 15:04")))
	}

	rtm.SendMessage(rtm.NewOutgoingMessage(report.String(), channelID))
}

// overdueChecker は定期的に2日以上返却されていないキーをチェックします。
func (b *Bot) overdueChecker() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		b.checkOverdue()
	}
}

// checkOverdue は2日以上返却されていないキーを検出し、通知を投稿します。
func (b *Bot) checkOverdue() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	now := time.Now()
	var overdueKeys []string

	for keyNum, status := range b.keyStatuses {
		if now.Sub(status.BorrowedAt) > 48*time.Hour {
			overdueKeys = append(overdueKeys, keyNum)
		}
	}

	if len(overdueKeys) > 0 {
		message := "以下のマスターキーが2日以上経過しても返却されていません:\n"
		for _, key := range overdueKeys {
			status := b.keyStatuses[key]
			userName, err := b.getUserName(status.Borrower)
			if err != nil {
				userName = status.Borrower
			}
			message += fmt.Sprintf("カード番号%s: %sさんが借りています。借りた日: %s\n",
				key, userName, status.BorrowedAt.Format("2006-01-02 15:04"))
		}
		b.api.PostMessage(b.channelID, slack.MsgOptionText(message, false))
	}
}

// getUserName はユーザーIDからユーザー名を取得します。
func (b *Bot) getUserName(userID string) (string, error) {
	user, err := b.api.GetUserInfo(userID)
	if err != nil {
		return "", err
	}
	return user.RealName, nil
}

func main() {
	// 環境変数を読み込む
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: .envファイルの読み込みに失敗しました。環境変数が設定されていることを確認してください。")
	}

	slackToken := os.Getenv("SLACK_BOT_TOKEN")
	if slackToken == "" {
		log.Fatal("環境変数 SLACK_BOT_TOKEN を設定してください")
	}

	api := slack.New(slackToken)
	listChannels(api) // デバッグ用: アクセス可能なチャンネル一覧を出力

	channelName := os.Getenv("CHANNEL_NAME")
	if channelName == "" {
		channelName = "general" // デフォルト値
	}

	bot, err := NewBot(slackToken, channelName)
	if err != nil {
		log.Fatalf("ボットの初期化に失敗しました: %v", err)
	}
	// トークンの検証
	authTest, err := api.AuthTest()
	if err != nil {
		log.Fatalf("AuthTest failed: %v", err)
	}

	fmt.Printf("Bot connected as: %s (ID: %s)\n", authTest.User, authTest.UserID)

	// ボットが参加しているチャンネルをリストアップ
	listChannels(api)

	log.Println("ボットを起動します...")
	bot.Run()
}
