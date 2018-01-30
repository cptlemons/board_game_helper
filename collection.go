package collections

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kylelemons/godebug/pretty"

	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/taskqueue"
	"google.golang.org/appengine/urlfetch"
)

func init() {
	http.HandleFunc("/list", getCollection)
	http.HandleFunc("/loadgame", loadGame)
}

type CollectionItem struct {
	ObjectID int `xml:"objectid,attr"`
}

type Collection struct {
	Items     []CollectionItem `xml:"item"`
	FetchTime time.Time        `xml:"-"`
}

type GameName struct {
	Name string `xml:"value,attr"`
	Type string `xml:"type,attr"`
}

type Result struct {
	NumPlayers string `xml:"numplayers,attr"`
	Votes      []struct {
		Num int `xml:"numvotes,attr"`
	} `xml:"result"`
}

type Poll struct {
	Name       string   `xml:"name,attr"`
	TotalVotes int      `xml:"totalvotes,attr"`
	Results    []Result `xml:"results"`
}

type GameXML struct {
	Names       []GameName `xml:"item>name"`
	PrimaryName string
	Description string `xml:"item>description"`
	MinPlayers  struct {
		Num int `xml:"value,attr"`
	} `xml:"item>minplayers"`
	MaxPlayers struct {
		Num int `xml:"value,attr"`
	} `xml:"item>maxplayers"`
	Polls     []*Poll   `xml:"item>poll"`
	FetchTime time.Time `xml:"-"`
}

type Game struct {
	Best      ([]int)
	Rec       ([]int)
	FetchTime time.Time `xml:"-"`
}

func getCollection(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	bggName := r.FormValue("bggName")
	var raw []byte
	collURL := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/collection?username=%s&excludesubtype=boardgameexpansion&own=1", bggName)

retry:
	resp, err := urlfetch.Client(ctx).Get(collURL)
	if err != nil {
		log.Fatalf("Failed to download url: %s", err)
	}
	if resp.StatusCode == http.StatusAccepted {
		fmt.Println("BGG request accepted, waiting for body")
		time.Sleep(10 * time.Second)
		goto retry
	}
	// TODO: BGG gives 200 on invalid username, write check to let user know they provided invalid name and to try again
	js, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read body: %s", err)
	}
	raw = js
	w.Header().Set("Content-Type", "text/plain")
	// fmt.Fprintf(w, "%s\n", raw)

	var coll Collection
	if err := xml.Unmarshal(raw, &coll); err != nil {
		log.Fatalf("Failed to unmarshal XML: %s", err)
	}
	coll.FetchTime = time.Now()
	pretty.Fprint(w, coll.FetchTime)

	key := datastore.NewKey(ctx, "Collection", bggName, 0, nil)

	if _, err := datastore.Put(ctx, key, &coll); err != nil {
		log.Fatalf("Failed to store user collection: %s", err)
	}

	var gameKeys []*datastore.Key
	for _, item := range coll.Items {
		gameKeys = append(gameKeys, datastore.NewKey(ctx, "Game", "", int64(item.ObjectID), nil))
	}

	earliestGame := time.Now().Add(-7 * 24 * time.Hour)
	games := make([]Game, len(gameKeys))
	datastore.GetMulti(ctx, gameKeys, games)
	// We don't need error handling because we expect some of these to fail -Kyle 29/01/2018
	var gameTasks []*taskqueue.Task
	for i := range games {
		game := games[i]
		if game.FetchTime.After(earliestGame) {
			continue
		}
		// fmt.Fprintf(w, "fetching game %d\n", coll.Items[i].ObjectID)
		gameTasks = append(gameTasks, taskqueue.NewPOSTTask("/loadgame", url.Values{
			"gameID": {fmt.Sprint(coll.Items[i].ObjectID)},
		}))
	}
	if _, err := taskqueue.AddMulti(ctx, gameTasks, ""); err != nil {
		log.Fatalf("Failed to queue game fetch tasks: %s", err)
	}
}

func loadGame(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	gameID := r.FormValue("gameID")
	gameXMLURL := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/thing?id=%s", gameID)
	//gameHTML := fmt.Sprintf("https://boardgamegeek.com/boardgame/%s", gameID)

	resp, err := urlfetch.Client(ctx).Get(gameXMLURL)
	if err != nil {
		log.Fatalf("Failed to download url: %s", err)
	}

	js, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read body: %s", err)
	}
	var raw []byte
	raw = js
	w.Header().Set("Content-Type", "text/plain")

	var gameXML GameXML
	if err := xml.Unmarshal(raw, &gameXML); err != nil {
		log.Fatalf("Failed to unmarshal XML: %s", err)
	}
	gameXML.FetchTime = time.Now()
	for _, name := range gameXML.Names {
		if name.Type == "primary" {
			gameXML.PrimaryName = name.Name
			break
		}
	}
	fmt.Fprintln(w, gameXML.PrimaryName)
	fmt.Fprintln(w, gameXML.Description)
	fmt.Fprintln(w, gameXML.MinPlayers)
	fmt.Fprintln(w, gameXML.MaxPlayers)
	pretty.Fprint(w, gameXML.Polls)
	fmt.Fprintln(w)

	var playerPoll *Poll
	for _, poll := range gameXML.Polls {
		switch poll.Name {
		case "suggested_numplayers":
			playerPoll = poll
			/*case "suggested_playerage":
				_agePoll = &poll
			case "language_dependence":
				_langPoll = &poll
			*/
		}
	}
	var bestAt, recAt ([]int)
	if playerPoll != nil {
		for _, playerCount := range playerPoll.Results {
			bestVotes, recVotes, nayVotes := playerCount.Votes[0].Num, playerCount.Votes[1].Num, playerCount.Votes[2].Num
			numPlayers, err := strconv.Atoi(playerCount.NumPlayers)
			if err != nil {
				numPlayers, err = strconv.Atoi(playerCount.NumPlayers[:len(playerCount.NumPlayers)-1])
				numPlayers += 1
			}
			fmt.Fprintln(w, bestVotes, recVotes, nayVotes, numPlayers)
			if bestVotes+recVotes > nayVotes {
				if bestVotes > recVotes {
					bestAt = append(bestAt, numPlayers)
				} else {
					recAt = append(recAt, numPlayers)
				}
			}
		}
	}
	pretty.Fprint(w, bestAt, recAt)
	var game Game
	game.Best = bestAt
	game.Rec = recAt
	game.FetchTime = time.Now()
	key := datastore.NewKey(ctx, "Games", gameID, 0, nil)

	if _, err := datastore.Put(ctx, key, &game); err != nil {
		log.Fatalf("Failed to store user collection: %s", err)
	}

}
