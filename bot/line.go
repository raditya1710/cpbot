package bot

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/azaky/cpbot/clist"
	"github.com/azaky/cpbot/repository"
	"github.com/azaky/cpbot/util"
	"github.com/line/line-bot-sdk-go/linebot"
)

type messageHandler func(linebot.Event, ...string)
type patternHandler struct {
	Pattern *regexp.Regexp
	Handler messageHandler
}
type LineBot struct {
	clistService *clist.Service
	client       *linebot.Client
	repo         *repository.Redis
	dailyTicker  *time.Ticker
	dailyTimer   map[string]*time.Timer
	dailyNext    time.Time
	dailyPeriod  time.Duration
	textPatterns []patternHandler
}

var (
	lineGreetingMessage     = os.Getenv("LINE_GREETING_MESSAGE")
	lineMaxMessageLength, _ = strconv.Atoi(os.Getenv("LINE_MAX_MESSAGE_LENGTH"))
	lineHelpString          = `Here are available commands:
@cpbot set daily HH:MM -> Set daily reminder for contests
@cpbot unset daily -> Turn off daily contest reminder
@cpbot set timezone Asia/Jakarta -> Set timezone
@cpbot in 3h30m -> Show contests starting in 3h30m
@cpbot help -> Show this`
)

func NewLineBot(channelSecret, channelToken string, clistService *clist.Service, redisEndpoint string) *LineBot {
	bot, err := linebot.New(channelSecret, channelToken)
	if err != nil {
		log.Fatalf("Error when initializing linebot: %s", err.Error())
	}
	repo := repository.NewRedis("line", redisEndpoint)
	b := &LineBot{
		clistService: clistService,
		client:       bot,
		repo:         repo,
	}
	b.registerTextPattern(`^\s*@cpbot\s+echo\s+(.*)$`, b.actionEcho)
	b.registerTextPattern(`^\s*@cpbot\s+in\s*(\S+)\s*$`, b.actionShowContestsWithin)
	b.registerTextPattern(`^\s*@cpbot\s+unset\s*daily\s*$`, b.actionRemoveDaily)
	b.registerTextPattern(`^\s*@cpbot\s+(?:set\s*)?daily\s*(\S+)\s*$`, b.actionUpdateDaily)
	b.registerTextPattern(`^\s*@cpbot\s+(?:set\s*)?timezone\s*(\S+)\s*$`, b.actionSetTimezone)
	b.registerTextPattern(`^\s*@cpbot\s+help\s*$`, b.actionShowHelp)
	b.registerTextPattern(`^\s*@cpbot\s*$`, b.actionShowHelp)
	return b
}

func (b *LineBot) registerTextPattern(regex string, handler messageHandler) {
	r, err := regexp.Compile(`(?i)` + regex)
	if err != nil {
		b.log("Error registering text pattern: %s", err.Error())
		return
	}
	b.textPatterns = append(b.textPatterns, patternHandler{
		Pattern: r,
		Handler: handler,
	})
}

func (b *LineBot) log(format string, args ...interface{}) {
	log.Printf("[LINE] "+format, args...)
}

func (b *LineBot) reply(event linebot.Event, messages ...string) error {
	var lineMessages []linebot.Message
	for _, message := range messages {
		lineMessages = append(lineMessages, linebot.NewTextMessage(message))
	}
	_, err := b.client.ReplyMessage(event.ReplyToken, lineMessages...).Do()
	if err != nil {
		b.log("Error replying to %+v: %s", event.Source, err.Error())
	}
	return err
}

func (b *LineBot) push(to string, messages ...string) error {
	var lineMessages []linebot.Message
	for _, message := range messages {
		lineMessages = append(lineMessages, linebot.NewTextMessage(message))
	}
	_, err := b.client.PushMessage(to, lineMessages...).Do()
	if err != nil {
		b.log("Error pushing to %s: %s", to, err.Error())
	}
	return err
}

func (b *LineBot) EventHandler(w http.ResponseWriter, req *http.Request) {
	events, err := b.client.ParseRequest(req)
	if err != nil {
		if err == linebot.ErrInvalidSignature {
			w.WriteHeader(400)
		} else {
			w.WriteHeader(500)
		}
		return
	}

	for _, event := range events {
		b.log("[EVENT][%s] Source: %#v", event.Type, event.Source)
		switch event.Type {

		case linebot.EventTypeJoin:
			fallthrough
		case linebot.EventTypeFollow:
			b.handleFollow(event)

		case linebot.EventTypeLeave:
			fallthrough
		case linebot.EventTypeUnfollow:
			b.handleUnfollow(event)

		case linebot.EventTypeMessage:
			switch message := event.Message.(type) {
			case *linebot.TextMessage:
				b.handleTextMessage(event, message)
			}
		}
	}
}

func (b *LineBot) generateGreetingMessage(tz *time.Location) []linebot.Message {
	var messages []linebot.Message
	messages = append(messages, linebot.NewTextMessage(lineGreetingMessage))

	initialReminder, err := generate24HUpcomingContestsMessage(b.clistService, tz, lineMaxMessageLength)
	if err == nil {
		for _, message := range initialReminder {
			messages = append(messages, linebot.NewTextMessage(message))
		}
	}

	return messages
}

func (b *LineBot) handleFollow(event linebot.Event) {
	user := util.LineEventSourceToString(event.Source)
	_, err := b.repo.AddUser(user)
	if err != nil {
		b.log("Error adding user: %s", err.Error())
	}

	tz, _ := b.repo.GetTimezone(user)

	messages := b.generateGreetingMessage(tz)
	if _, err = b.client.ReplyMessage(event.ReplyToken, messages...).Do(); err != nil {
		b.log("Error replying to follow event: %s", err.Error())
	}

	// Setup default daily reminder
	t, _ := util.ParseTime(os.Getenv("LINE_DAILY_DEFAULT"))
	b.updateDaily(user, t)
}

func (b *LineBot) handleUnfollow(event linebot.Event) {
	user := util.LineEventSourceToString(event.Source)
	_, err := b.repo.RemoveUser(user)
	if err != nil {
		b.log("Error removing user: %s", err.Error())
	}
}

func (b *LineBot) handleTextMessage(event linebot.Event, message *linebot.TextMessage) {
	log.Printf("Received message from %s: %s", event.Source.UserID, message.Text)
	for _, p := range b.textPatterns {
		matches := p.Pattern.FindStringSubmatch(message.Text)
		if matches != nil {
			p.Handler(event, matches...)
			return
		}
	}
}

func (b *LineBot) actionEcho(event linebot.Event, args ...string) {
	b.reply(event, args[1])
}

func (b *LineBot) actionShowHelp(event linebot.Event, args ...string) {
	b.reply(event, lineHelpString)
}

func (b *LineBot) actionShowContestsWithin(event linebot.Event, args ...string) {
	duration, err := time.ParseDuration(args[1])
	if err != nil {
		// Duration is not valid
		reply := fmt.Sprintf("%s is not a valid duration", args[1])
		b.reply(event, reply)
		return
	}

	user := util.LineEventSourceToString(event.Source)
	tz, _ := b.repo.GetTimezone(user)

	replies, err := generateUpcomingContestsMessage(b.clistService, time.Now(), time.Now().Add(duration), tz, fmt.Sprintf("Contests starting within %s:", duration), lineMaxMessageLength)
	if err != nil {
		b.log("Error getting contests: %s", err.Error())
		return
	}

	b.reply(event, replies...)
}

func (b *LineBot) actionUpdateDaily(event linebot.Event, args ...string) {
	tstr := args[1]
	user := util.LineEventSourceToString(event.Source)
	tz, _ := b.repo.GetTimezone(user)

	t, err := util.ParseTimeInLocation(tstr, tz)
	if err != nil {
		reply := fmt.Sprintf("%s is not a valid time", tstr)
		b.reply(event, reply)
		return
	}

	b.updateDaily(user, t)
	reply := fmt.Sprintf("Daily contest reminder has been set everyday at %s", tstr)
	b.reply(event, reply)
}

func (b *LineBot) actionRemoveDaily(event linebot.Event, args ...string) {
	user := util.LineEventSourceToString(event.Source)
	b.removeDaily(user)
	reply := "Daily contest reminder has been turned off"
	b.reply(event, reply)
}

func (b *LineBot) actionSetTimezone(event linebot.Event, args ...string) {
	tz := args[1]
	user := util.LineEventSourceToString(event.Source)
	_, err := util.LoadLocation(tz)
	if err != nil {
		reply := fmt.Sprintf("%s is not a valid timezone. Timezone is not changed", tz)
		b.reply(event, reply)
		return
	}
	b.repo.SetTimezone(user, tz)
	reply := fmt.Sprintf("Timezone is set to %s", tz)
	b.reply(event, reply)
}

func (b *LineBot) StartDailyJob(duration time.Duration) {
	if b.dailyTicker != nil {
		b.log("An attempt to start daily job, but the job has already started")
		return
	}
	b.dailyPeriod = duration
	b.dailyTicker = time.NewTicker(b.dailyPeriod)

	b.dailyJob(time.Now())
	go func() {
		for t := range b.dailyTicker.C {
			b.dailyJob(t)
		}
	}()
}

func (b *LineBot) dailyJob(now time.Time) {
	b.log("[DAILY] Start job")
	b.dailyNext = now.Add(b.dailyPeriod)

	userTimes, err := b.repo.GetDailyWithin(now, b.dailyNext)
	if err != nil {
		b.log("[DAILY] Error getting daily within: %s", err.Error())
		return
	}

	b.log("[DAILY] Schedule for the following users: %v", userTimes)

	b.dailyTimer = make(map[string]*time.Timer)
	for _, userTime := range userTimes {
		tz, _ := b.repo.GetTimezone(userTime.User)
		next := util.NextTime(userTime.Time)
		b.dailyTimer[userTime.User] = time.AfterFunc(next.Sub(time.Now()), b.dailyReminderFunc(userTime.User, tz))
	}
}

func (b *LineBot) dailyStarted() bool {
	return b.dailyTicker != nil
}

func (b *LineBot) updateDaily(user string, t int) {
	tz, _ := b.repo.GetTimezone(user)

	_, err := b.repo.AddDaily(user, t)
	if err != nil {
		b.log("[DAILY] Error adding to repo (%s, %d): %s", user, t, err.Error())
	}

	if !b.dailyStarted() {
		return
	}

	if t, ok := b.dailyTimer[user]; ok {
		t.Stop()
		delete(b.dailyTimer, user)
	}

	next := util.NextTime(t)
	if next.Before(b.dailyNext) {
		b.dailyTimer[user] = time.AfterFunc(next.Sub(time.Now()), b.dailyReminderFunc(user, tz))
	}
}

func (b *LineBot) removeDaily(user string) {
	_, err := b.repo.RemoveDaily(user)
	if err != nil {
		b.log("[DAILY] Error removing from repo (%s): %s", user, err.Error())
	}

	if !b.dailyStarted() {
		return
	}

	if t, ok := b.dailyTimer[user]; ok {
		t.Stop()
		delete(b.dailyTimer, user)
	}
}

func (b *LineBot) dailyReminderFunc(user string, tz *time.Location) func() {
	return func() {
		messages, err := generate24HUpcomingContestsMessage(b.clistService, tz, lineMaxMessageLength)
		if err != nil {
			// TODO: retry mechanism
			b.log("[DAILY] Error generating message: %s", err.Error())
			return
		}

		eventSource, err := util.StringToLineEventSource(user)
		if err != nil {
			b.log("[DAILY] found invalid user [%s]: %s", user, err.Error())
			return
		}
		b.push(util.LineEventSourceToReplyString(eventSource), messages...)
	}
}
