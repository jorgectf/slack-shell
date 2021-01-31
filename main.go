package main

import (
	"bufio"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	. "github.com/gagliardetto/utilz"
	"github.com/slack-go/slack"
	"github.com/urfave/cli/v2"
)

// main func
func main() {
	var configFilePath string
	var displayUnredacted bool
	var displayToken string
	var isDebug bool
	var noStdout bool
	var noStderr bool
	var waitDuration time.Duration

	// urfave/cli declaration
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "config",
				Aliases:     []string{"c"},
				Value:       "config.json",
				Usage:       "Path to configuration `FILE`",
				EnvVars:     []string{"slack-shell-config"},
				Destination: &configFilePath,
			},
			&cli.BoolFlag{
				Name:        "displayUnredacted",
				Aliases:     []string{"dU"},
				Value:       false,
				Usage:       "Display Slack Token unredacted (Otherwise make sure it is loaded)",
				EnvVars:     []string{"slack-shell-config"},
				Destination: &displayUnredacted,
			},
			&cli.DurationFlag{
				Name:        "wait",
				Value:       5 * time.Second,
				Usage:       "Wait duration between requests.",
				Destination: &waitDuration,
			},
			&cli.BoolFlag{
				Name:        "debug",
				Aliases:     []string{"d"},
				Value:       false,
				Usage:       "Debug mode",
				Destination: &isDebug,
			},
			&cli.BoolFlag{
				Name:        "noStdout",
				Aliases:     []string{"nO"},
				Value:       false,
				Usage:       "Do not receive StdOut.",
				Destination: &noStdout,
			},
			&cli.BoolFlag{
				Name:        "noStderr",
				Aliases:     []string{"nE"},
				Value:       false,
				Usage:       "Do not receive StdErr",
				Destination: &noStderr,
			},
		},
		Action: func(c *cli.Context) error {
			Infof("Using %s as config file...", configFilePath)

			conf, err := LoadConfigFromFile(configFilePath)
			if err != nil {
				panic(err)
			}

			// validate and change struct name if more fields needed

			if !c.Bool("displayUnredacted") {
				displayToken = GetRedacted(conf.SlackToken)
			} else {
				displayToken = conf.SlackToken
			}
			Infof("Using %s as Slack Token", displayToken)

			if noStdout && noStderr {
				panic(
					fmt.Errorf("Cannot set noStdout and noStderr at the same time."),
				)
			}

			api := slack.New(conf.SlackToken)
			rtm := api.NewRTM()

			go rtm.ManageConnection()

			for msg := range rtm.IncomingEvents {
				switch ev := msg.Data.(type) {

				case *slack.DesktopNotificationEvent:
					// set as argument not to access the global variable
					go func(ev *slack.DesktopNotificationEvent) {
						fmt.Printf("Desktop Notification: %v\n", ev)

						command, err := ParseMessage(ev.Content)
						if err != nil {
							panic(err)
						}

						toSend := fmt.Sprintf("Received: %s\n\n", ev.Content)

						// send initial message
						_, msgTimestamp, err := rtm.PostMessage(ev.Channel, slack.MsgOptionText(toSend, false))
						if err != nil {
							panic(err)
						}

						splitCommand := strings.Split(command, " ")
						cmd := exec.Command(
							splitCommand[0], splitCommand[1:]...,
						)

						// Sync stdout and stderr (Not to mess up the order)
						stdoutFinished := true
						if !noStdout {
							stdout, err := cmd.StdoutPipe()
							stdoutFinished = false
							if err != nil {
								log.Fatal(err)
							}

							go func() {
								buf := bufio.NewReader(stdout)
								for {
									line, _, err := buf.ReadLine()
									if err != nil {
										break
									}
									toSend += string(line) + "\n"
								}
								stdoutFinished = true
							}()
						}

						stderrFinished := true
						if !noStderr {
							stderr, err := cmd.StderrPipe()
							stderrFinished = false
							if err != nil {
								log.Fatal(err)
							}

							go func() {
								buf := bufio.NewReader(stderr)
								for {
									line, _, err := buf.ReadLine()
									if err != nil {
										break
									}
									toSend += string(line) + "\n"
								}
								stderrFinished = true
							}()
						}

						// must cmd.Start() *after* Std(out|err)Pipe()
						err = cmd.Start()
						if err != nil {
							panic(err)
						}

						for {
							rtm.UpdateMessage(ev.Channel, msgTimestamp, slack.MsgOptionText("```"+toSend+"```", false))
							time.Sleep(waitDuration)

							if stdoutFinished && stderrFinished {
								toSend += "\n" + "Command finished"
								rtm.UpdateMessage(ev.Channel, msgTimestamp, slack.MsgOptionText("```"+toSend+"```", false))
								break
							}
						}

					}(ev)

				case *slack.RTMError:
					fmt.Printf("Error: %s\n", ev.Error())

				case *slack.InvalidAuthEvent:
					fmt.Printf("Invalid credentials")
					return nil

				default:
					if isDebug {
						fmt.Printf("Unexpected: %v\n%s\n", msg.Data, ev)
					}
				}
			}

			return nil
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

// utils
type TokenFileConfig struct {
	SlackToken string `json:"slack-token"`
}

func LoadConfigFromFile(filepath string) (*TokenFileConfig, error) {
	jsonFile, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("error while reading config file from %q: %s", filepath, err)
	}

	var conf TokenFileConfig
	err = json.Unmarshal(jsonFile, &conf)
	if err != nil {
		return nil, fmt.Errorf("error while unmarshaling config file: %s", err)
	}

	return &conf, nil
}

func GetRedacted(unRedactedToken string) string {
	// redact any letter & digit
	pattern := regexp.MustCompile(`[A-Za-z0-9]`)
	return pattern.ReplaceAllString(unRedactedToken, "X")
}

func ParseMessage(message string) (string, error) {
	// jorgectf: @slackshellapp this is a command

	// https://api.slack.com/reference/surfaces/formatting#escaping
	message = strings.ReplaceAll(message, "&amp;", "&")
	message = strings.ReplaceAll(message, "&lt;", "<")
	message = strings.ReplaceAll(message, "&gt;", ">")

	// copy-pasting from slack -> jorgectf: @slackshellapp%C2%A0this
	urlEncodedMessage := url.QueryEscape(message)
	if strings.Contains(urlEncodedMessage, "%C2%A0") {
		message, _ = url.QueryUnescape(
			strings.ReplaceAll(urlEncodedMessage, "%C2%A0", "+"),
		)
	}

	// get "this is a command"
	message = strings.Join(
		// get [this is a command]
		strings.Split(message, " ")[2:],
		" ",
	)
	if message == "" {
		return "", fmt.Errorf("Empty command received. %s", message)
	}

	// convert to base64
	message = b64.StdEncoding.EncodeToString([]byte(message))
	// http://www.jackson-t.ca/runtime-exec-payloads.html
	message = fmt.Sprintf("bash -c {echo,%s}|{base64,-d}|bash", message)

	return message, nil
}
