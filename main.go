package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-yaml/yaml"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/totegamma/httpsig"

	apmiddleware "github.com/concrnt/ccworld-ap-bridge/middleware"
	"github.com/concrnt/ccworld-ap-bridge/types"
	"github.com/totegamma/concurrent/client"
	"github.com/totegamma/concurrent/core"
)

const (
	UserAgent = "ccworld-ap-relay/0.1"
)

type Config struct {
	FQDN         string   `yaml:"fqdn"`
	PrivateKey   string   `yaml:"private_key"`
	PublicKey    string   `yaml:"public_key"`
	Destinations []string `yaml:"destinations"`
	Source       string   `yaml:"source"`
}

var config Config

func main() {

	log.Println("Starting ccworld-ap-relay")

	f, err := os.Open("/etc/ccworld-ap-relay.yaml")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	err = yaml.NewDecoder(f).Decode(&config)
	if err != nil {
		panic(err)
	}

	log.Println("Config Loaded! FQDN:", config.FQDN)

	e := echo.New()
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Binder = &apmiddleware.Binder{}

	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})

	e.GET("/.well-known/nodeinfo", WellKnownNodeInfo)
	e.GET("/nodeinfo/2.0", NodeInfo)
	e.GET("/actor", AcctRelay)

	e.POST("/inbox", Inbox)

	log.Println("subscribing to", config.Source)
	go SubscribeTimeline(config.Source)

	port := ":8000"
	envport := os.Getenv("CCWORLD_AP_RELAY_PORT")
	if envport != "" {
		port = ":" + envport
	}

	log.Println("Starting server on", port)
	e.Logger.Fatal(e.Start(port))
}

func SubscribeTimeline(id string) {

	ctx := context.Background()

	split := strings.Split(id, "@")
	if len(split) != 2 {
		panic("Invalid source ID")
	}

	domain := split[1]

	u := url.URL{Scheme: "wss", Host: domain, Path: "/api/v1/timelines/realtime"}
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	c, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		log.Println("websocket.Dial:", err)
		return
	}

	c.WriteJSON(map[string]interface{}{
		"type":     "listen",
		"channels": []string{id},
	})

	fmt.Println("Subscribed to", id)

	ccc := client.NewClient()

	pingTicker := time.NewTicker(10 * time.Second)
	defer pingTicker.Stop()

	go func() {
		for range pingTicker.C {
			err := c.WriteMessage(websocket.PingMessage, nil)
			if err != nil {
				log.Println("websocket.PingMessage:", err)
				return
			}
		}
	}()

	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			log.Println("websocket.ReadMessage:", err)
			panic(err)
		}

		var event core.Event
		err = json.Unmarshal(message, &event)
		if err != nil {
			log.Println("json.Unmarshal:", err)
			continue
		}

		if event.Timeline != id {
			continue
		}

		if event.Item.ResourceID[0] != 'm' {
			continue
		}

		entity, err := ccc.GetEntity(ctx, domain, event.Item.Owner, nil)
		if err != nil {
			log.Println("GetEntity:", err)
			continue
		}

		apURL := "https://" + entity.Domain + "/ap/note/" + event.Item.ResourceID
		err = Announce(ctx, apURL)
		if err != nil {
			log.Println("Announce:", err)
			continue
		}

		jsonPrint("Event", event)
	}

}

func Announce(ctx context.Context, objectURL string) error {

	announce := types.ApObject{
		Context: []string{"https://www.w3.org/ns/activitystreams"},
		Type:    "Announce",
		ID:      "https://" + config.FQDN + "/note/" + url.PathEscape(objectURL) + "/activity",
		Actor:   "https://" + config.FQDN + "/actor",
		Content: "",
		Object:  objectURL,
	}

	announceBytes, err := json.Marshal(announce)
	if err != nil {
		log.Println("api/handler/announce json.Marshal:", err)
		return err
	}

	for _, dest := range config.Destinations {
		err = PostToInbox(ctx, dest, announceBytes)
		if err != nil {
			log.Println("api/handler/announce PostToInbox:", err)
			continue
		}
	}

	return nil
}

func Inbox(c echo.Context) error {
	ctx := c.Request().Context()

	var object types.ApObject
	err := c.Bind(&object)
	if err != nil {
		log.Printf("api/handler/inbox %v", err)
		return c.String(http.StatusBadRequest, "Invalid request body")
	}

	switch object.Type {
	case "Follow":

		requester, err := FetchPerson(ctx, object.Actor)
		if err != nil {
			log.Println("ap/service/inbox/follow FetchPerson:", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "FetchPerson Error"})
		}

		accept := types.ApObject{
			Context: "https://www.w3.org/ns/activitystreams",
			ID:      "https://" + config.FQDN + "/actor/follows/" + url.PathEscape(requester.ID),
			Type:    "Accept",
			Actor:   "https://" + config.FQDN + "/actor",
			Object:  object,
		}

		acceptBytes, err := json.Marshal(accept)
		if err != nil {
			log.Println("ap/service/inbox/follow json.Marshal:", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "json.Marshal Error"})
		}

		err = PostToInbox(ctx, requester.Inbox, acceptBytes)
		if err != nil {
			log.Println("ap/service/inbox/follow PostToInbox:", err)
			return c.JSON(http.StatusOK, echo.Map{"error": "PostToInbox Error"})
		}

		return c.JSON(http.StatusOK, echo.Map{"success": "Follow Accepted"})

	default:
		// print request body
		jsonPrint("Unhandled Activitypub Object", object)
		return c.JSON(http.StatusOK, echo.Map{"error": "Unhandled Activitypub Object"})
	}
}

func jsonPrint(title string, v interface{}) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("----- : " + title + " : -----")
	fmt.Println(string(b))
	fmt.Println("--------------------------------")
}

func FetchPerson(ctx context.Context, actor string) (types.ApObject, error) {

	var person types.ApObject
	req, err := http.NewRequest("GET", actor, nil)
	if err != nil {
		return person, err
	}

	req.Header.Set("Accept", "application/activity+json")
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Host", req.URL.Host)
	client := new(http.Client)

	resp, err := client.Do(req)
	if err != nil {
		return person, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	err = json.Unmarshal(body, &person)
	if err != nil {
		return person, err
	}

	return person, nil
}

func PostToInbox(ctx context.Context, inbox string, objectBytes []byte) error {

	req, err := http.NewRequest("POST", inbox, bytes.NewBuffer(objectBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("Host", req.URL.Host)
	client := new(http.Client)

	prefs := []httpsig.Algorithm{httpsig.RSA_SHA256}
	digestAlgorithm := httpsig.DigestSha256
	headersToSign := []string{httpsig.RequestTarget, "date", "digest", "host"}
	signer, _, err := httpsig.NewSigner(prefs, digestAlgorithm, headersToSign, httpsig.Signature, 0)
	if err != nil {
		log.Println(err)
		return err
	}

	block, _ := pem.Decode([]byte(config.PrivateKey))
	if block == nil {
		return fmt.Errorf("failed to decode PEM block containing private key")
	}

	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return err
	}

	err = signer.SignRequest(priv, "https://"+config.FQDN+"/actor#main-key", req, objectBytes)
	if err != nil {
		log.Println(err)
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
	}
	log.Printf("POST %s [%d]: %s", inbox, resp.StatusCode, string(body))

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("error posting to inbox: %d", resp.StatusCode)
	}

	defer resp.Body.Close()

	return nil
}

func WellKnownNodeInfo(c echo.Context) error {
	c.Response().Header().Set("Content-Type", "application/json")
	return c.JSON(http.StatusOK, types.WellKnown{
		Links: []types.WellKnownLink{
			{
				Rel:  "http://nodeinfo.diaspora.software/ns/schema/2.0",
				Href: "https://" + config.FQDN + "/nodeinfo/2.0",
			},
		},
	})
}

func NodeInfo(c echo.Context) error {
	c.Response().Header().Set("Content-Type", "application/json")
	return c.JSON(http.StatusOK, types.NodeInfo{
		Version: "2.0",
		Software: types.NodeInfoSoftware{
			Name:    "ccworld-ap-relay",
			Version: "0.1",
		},
		Protocols: []string{
			"activitypub",
		},
		OpenRegistrations: false,
		Metadata: types.NodeInfoMetadata{
			NodeName:        "ccworld-ap-relay",
			NodeDescription: "relay service for ccworld",
			Maintainer: types.NodeInfoMetadataMaintainer{
				Name: "totegamma",
			},
			ThemeColor: "#000000",
		},
	})
}

func AcctRelay(c echo.Context) error {
	c.Response().Header().Set("Content-Type", "application/activity+json")
	return c.JSON(http.StatusOK, types.ApObject{
		Context:           "https://www.w3.org/ns/activitystreams",
		Type:              "Application",
		PreferredUsername: "relay",
		ID:                "https://" + config.FQDN + "/actor",
		Inbox:             "https://" + config.FQDN + "/actor/inbox",
		Outbox:            "https://" + config.FQDN + "/actor/outbox",
		Following:         "https://" + config.FQDN + "/actor/following",
		Followers:         "https://" + config.FQDN + "/actor/followers",
		PublicKey: &types.Key{
			ID:           "https://" + config.FQDN + "/actor#main-key",
			Type:         "Key",
			Owner:        "https://" + config.FQDN + "/actor",
			PublicKeyPem: config.PublicKey,
		},
	})
}
