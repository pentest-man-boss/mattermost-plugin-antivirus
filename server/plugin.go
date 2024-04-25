package main

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pkg/errors"

	pluginapi "github.com/mattermost/mattermost-plugin-api"

	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/plugin"

	"github.com/dutchcoders/go-clamd"
)

type Plugin struct {
	plugin.MattermostPlugin

	configurationLock sync.RWMutex

	configuration *configuration

	client *pluginapi.Client

	botUserID string
}

func (p *Plugin) FileWillBeUploaded(_ *plugin.Context, info *model.FileInfo, file io.Reader, _ io.Writer) (*model.FileInfo, string) {
	config := p.getConfiguration()

	var av *clamd.Clamd
	if config.ConnectionType == "tcp" {
		av = clamd.NewClamd("tcp://" + config.ClamavHostPort)
	} else {
		av = clamd.NewClamd(config.ClamavSocketPath)
	}
	abortScan := make(chan bool)
	response, err := av.ScanStream(file, abortScan)
	if err != nil {
		p.API.LogError("Error while scanning for viruses. " + err.Error())
		return nil, "File Scanning Server unreachable, contact your Mattermost administrator for assistance."
	}
	for {
		select {
		case scanResult, ok := <-response:
			if !ok {
				return info, ""
			}
			if scanResult.Status != clamd.RES_OK {
				p.API.LogWarn("The antivirus service would not allow you to attach this file.", "filename", info.Name, "user", info.CreatorId, "scan_result", scanResult.Raw)
				return nil, "The antivirus service did not allow you to attach this file."
			}
			continue
		case <-time.After(time.Duration(config.ScanTimeoutSeconds) * time.Second):
			close(abortScan)
			p.API.LogError("Scan timed out.", "filename", info.Name)
			return nil, "Problem with antivirus scanner."
		}
	}
}

// ServeHTTP demonstrates a plugin that handles HTTP requests by greeting the world.
func (p *Plugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "Hello, world!")
}

func (p *Plugin) OnActivate() error {
	command, _ := p.getCommand()
	err := p.MattermostPlugin.API.RegisterCommand(command)
	if err != nil {
		return errors.Wrap(err, "couldn't register command")
	}

	p.client = pluginapi.NewClient(p.MattermostPlugin.API, p.MattermostPlugin.Driver)
	botUserID, err := p.client.Bot.EnsureBot(&model.Bot{
		Username:    "antivirus",
		DisplayName: "Antivirus",
		Description: "Antivirus plugin for scanning uploaded files.",
	})
	if err != nil {
		return errors.Wrap(err, "failed to ensure bot account")
	}
	p.botUserID = botUserID

	return nil
}
