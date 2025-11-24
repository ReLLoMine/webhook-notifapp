package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"iter"
	"log"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/redis/go-redis/v9"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type Environment struct {
	botToken     string
	serverHost   string
	serverPort   string
	dbURI        string
	dbUsername   string
	dbPassword   string
	templatesDir string
}

var env Environment = Environment{
	botToken:     getEnv("BOT_TOKEN", "token"),
	serverHost:   getEnv("SERVER_HOST", "0.0.0.0"),
	serverPort:   getEnv("SERVER_PORT", "8080"),
	dbURI:        getEnv("DB_URI", "127.0.0.1:6379"),
	dbUsername:   getEnv("DB_USERNAME", ""),
	dbPassword:   getEnv("DB_PASSWORD", ""),
	templatesDir: getEnv("TEMPLATES_DIR", "./templates"),
}

var rclient *redis.Client
var mainContext context.Context
var tbot *bot.Bot
var templates *template.Template

func getEnv(envVar string, fallback string) string {
	if env := os.Getenv(envVar); env != "" {
		return env
	}
	return fallback
}

type Alert struct {
	Labels       Labels      `json:"labels"`
	Annotations  Annotations `json:"annotations"`
	GeneratorURL string      `json:"generatorURL"`
}

type Labels struct {
	Severity string `json:"severity"`
}

type Annotations struct {
	Description string `json:"description"`
	Summary     string `json:"summary"`
}

var severity map[string]string = map[string]string{
	"warning":  "⚠️",
	"info":     "ℹ️",
	"critical": "⛔",
}

type Message struct {
	Alerts []Alert `json:"alerts"`
	Status string  `json:"status"`
}

type MessageComposed struct {
	Severity     string
	SeverityIcon string
	Status       string
	Summary      string
	Description  string
	GeneratorURL string
}

func (msg *Message) Format() string {

	result := ""

	for _, alert := range msg.Alerts {
		var header string = alert.Labels.Severity
		if value, ok := severity[alert.Labels.Severity]; ok {
			header = fmt.Sprintf("%s <b>%s</b> %s", value, cases.Title(language.English, cases.Compact).String(alert.Labels.Severity), value)
		}
		result += fmt.Sprintf("%s\nMessage: <blockquote>%s</blockquote>\n---\n<blockquote>%s</blockquote>\n<a href=\"%s\">Metric that caused alert</a>", header, alert.Annotations.Summary, alert.Annotations.Description, alert.GeneratorURL)
	}
	return result
}

func (msg *Message) ComposeMessage() []MessageComposed {
	var res []MessageComposed = make([]MessageComposed, 0)
	for _, alert := range msg.Alerts {
		icon, ok := severity[alert.Labels.Severity]
		if !ok {
			icon = ""
		}
		res = append(res, MessageComposed{
			Severity:     cases.Title(language.English, cases.Compact).String(alert.Labels.Severity),
			SeverityIcon: icon,
			Status:       msg.Status,
			Summary:      alert.Annotations.Summary,
			Description:  alert.Annotations.Description,
			GeneratorURL: alert.GeneratorURL,
		})
	}
	return res
}

func loadTemplates() {
	files, _ := os.ReadDir(env.templatesDir)

	filesNames := make([]string, 0)
	for i := 0; i < len(files); i++ {
		if files[i].IsDir() {
			continue
		}
		filesNames = append(filesNames, env.templatesDir+string(os.PathSeparator)+files[i].Name())
	}

	var err error
	templates, err = template.ParseFiles(filesNames...)
	if err != nil {
		log.Panic(err.Error())
	}
}

func (msg *Message) AllAlerts() iter.Seq[string] {
	compMsgs := msg.ComposeMessage()
	return func(yield func(string) bool) {
		for _, compMsg := range compMsgs {
			var buf bytes.Buffer
			err := templates.ExecuteTemplate(&buf, "message-template", compMsg)
			if err != nil {
				log.Panic(err.Error())
			}

			if !yield(buf.String()) {
				return
			}
		}
	}
}

func init() {
	loadTemplates()
}

func init() {
	mainContext = context.Background()
	ctx, cancel := context.WithTimeout(mainContext, time.Second*5)
	defer cancel()

	rclient = redis.NewClient(&redis.Options{
		Addr:     env.dbURI,
		Username: env.dbUsername,
		Password: env.dbPassword,
	})

	if err := rclient.Ping(ctx).Err(); err != nil {
		log.Fatal(err.Error())
	}
}

func init() {
	opts := []bot.Option{
		bot.WithDefaultHandler(botHandler),
	}

	var err error
	tbot, err = bot.New(env.botToken, opts...)
	if err != nil {
		log.Fatal(err.Error())
	}

	go func() {
		log.Print("Starting bot")
		tbot.Start(mainContext)
		log.Print("Stoped bot")
	}()

}

func mainHandler(rw http.ResponseWriter, req *http.Request) {
	path := req.RequestURI
	log.Printf("Action %s to %s from %s", req.Method, path, req.RemoteAddr)

	if req.Method != http.MethodPost {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	var msg Message

	if err := json.NewDecoder(req.Body).Decode(&msg); err != nil {
		log.Panic(err.Error())
	}

	log.Printf("Recieved message:\n%v", msg)

	ctx, cancel := context.WithTimeout(mainContext, time.Second*2)
	defer cancel()

	subscribers, err := rclient.LRange(ctx, "subscribers", 0, -1).Result()
	if err != nil {
		log.Panic(err.Error())
	}

	for alert := range msg.AllAlerts() {
		for _, subscriber := range subscribers {
			if _, err := tbot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:    subscriber,
				Text:      alert,
				ParseMode: models.ParseModeHTML,
			}); err != nil {
				rw.WriteHeader(http.StatusInternalServerError)
				log.Panic(err.Error())
			}
		}
	}

	rw.WriteHeader(http.StatusOK)
}

func botHandler(ctx context.Context, b *bot.Bot, update *models.Update) {

	resdisctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var response string

	switch update.Message.Text {
	case "/unsubscribe":
		rclient.LRem(resdisctx, "subscribers", 0, update.Message.Chat.ID)
		response = "Succesfully unsubscribed"
	case "/subscribe":
		subscribers, err := rclient.LRange(resdisctx, "subscribers", 0, -1).Result()
		if !slices.Contains(subscribers, fmt.Sprint(update.Message.Chat.ID)) || err != nil {
			rclient.RPush(resdisctx, "subscribers", update.Message.Chat.ID)
		}
		response = "Succesfully subscribed"
	default:
		response = "Unknown command.\nUse /subscribe or /unsubscribe"
	}

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   response,
	})
}

func main() {
	http.HandleFunc("/", mainHandler)

	log.Printf("Runnging go server on %s:%s \n", env.serverHost, env.serverPort)
	log.Printf("Redis on %s\n", env.dbURI)

	if err := http.ListenAndServe(env.serverHost+":"+env.serverPort, nil); err != nil {
		log.Fatal(err.Error())
	}
}
