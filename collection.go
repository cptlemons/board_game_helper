package collections

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kylelemons/godebug/pretty"

	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/taskqueue"
	"google.golang.org/appengine/urlfetch"
)

func init() {
	http.HandleFunc("/", home)
	http.HandleFunc("/list", getCollection)
	http.HandleFunc("/loadgame", loadGame)
	http.HandleFunc("/watchprogress", watchProgress)
	http.HandleFunc("/suggestedgames", suggestedGames)
	http.HandleFunc("/logs", displayLogs)
}

type CollectionItem struct {
	ObjectID string `xml:"objectid,attr"`
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
	PrimaryName string     `xml:"-"`
	Description string     `xml:"item>description"`
	MinPlayers  struct {
		Num int `xml:"value,attr"`
	} `xml:"item>minplayers"`
	MaxPlayers struct {
		Num int `xml:"value,attr"`
	} `xml:"item>maxplayers"`
	Polls     []*Poll   `xml:"item>poll"`
	FetchTime time.Time `xml:"-"`
}

type GameJSON struct {
	Score   float64 `json:"average,string"`
	Weight  float64 `json:"avgweight,string"`
	BScore  float64 `json:"baverage,string"`
	Ratings int     `json:"usersrated,string"`
}

type Game struct {
	Name       string
	Best       []int
	Rec        []int
	MinPlayers int
	MaxPlayers int
	Score      float64
	Weight     float64
	BScore     float64
	Ratings    int
	FetchTime  time.Time `xml:"-"`
}

func home(w http.ResponseWriter, r *http.Request) {
	page := `<form action="/list" method="post">
    <div>
        BGG Username: <input type="text" name="bggName"><input type="submit" value="Load Collection">
    </div>
</form>
<form action="/loadgame" method="post">
    <div>
        Game ID: <input type="text" name="gameID"><input type="submit" value="Load Game">
    </div>
</form>
<form action="/suggestedgames" method="post">
    <div>
        Num Players: <input type="text" name="numPlayers"> Collection: <input type="text" name="bggName"><input type="submit" value="View Recs"><br>
    </div>
</form>`
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, page)
}

func getCollection(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	bggName := r.FormValue("bggName")
	collURL := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/collection?username=%s&excludesubtype=boardgameexpansion&own=1", bggName)

retry:
	resp, err := urlfetch.Client(ctx).Get(collURL)
	if err != nil {
		log.Errorf(ctx, "Failed to download bgg collection: %s", err)
		http.Error(w, "An error occurred. Try again.", http.StatusInternalServerError)
		return
	}
	if resp.StatusCode == http.StatusAccepted {
		log.Infof(ctx, "BGG request accepted, waiting for body")
		time.Sleep(10 * time.Second)
		goto retry
	}
	// TODO: BGG gives 200 on invalid username, write check to let user know they provided invalid name and to try again
	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Errorf(ctx, "Failed to read collection body: %s", err)
		http.Error(w, "An error occurred. Try again.", http.StatusInternalServerError)
		return
	}

	var coll Collection
	if err := xml.Unmarshal(raw, &coll); err != nil {
		log.Errorf(ctx, "Failed to unmarshal XML: %s", err)
		http.Error(w, "An error occurred. Try again.", http.StatusInternalServerError)
		return
	}
	coll.FetchTime = time.Now()

	key := datastore.NewKey(ctx, "Collection", bggName, 0, nil)

	if _, err := datastore.Put(ctx, key, &coll); err != nil {
		log.Errorf(ctx, "Failed to store user collection: %s", err)
		http.Error(w, "An error occurred. Try again.", http.StatusInternalServerError)
		return
	}
	log.Infof(ctx, "Datastore put of collection successful")

	var gameKeys []*datastore.Key
	for _, item := range coll.Items {
		gameKeys = append(gameKeys, datastore.NewKey(ctx, "Game", item.ObjectID, 0, nil))
	}
	earliestGame := time.Now().Add(-7 * 24 * time.Hour)
	games := make([]Game, len(gameKeys))
	datastore.GetMulti(ctx, gameKeys, games)
	// We don't need error handling because we expect some of these to fail -Kyle 29/01/2018
	var gameTasks []*taskqueue.Task
	for i := range games {
		game := games[i]
		if game.FetchTime.After(earliestGame) {
			log.Infof(ctx, "Skipping %v with time %s\n", game, game.FetchTime)
			continue
		}
		gameTasks = append(gameTasks, taskqueue.NewPOSTTask("/loadgame", url.Values{
			"gameID": {fmt.Sprint(coll.Items[i].ObjectID)},
		}))
	}
	if _, err := taskqueue.AddMulti(ctx, gameTasks, "my-push-queue"); err != nil {
		log.Errorf(ctx, "Failed to queue game fetch tasks: %s", err)
		http.Error(w, "An error occurred. Try again.", http.StatusInternalServerError)
		return
	}
	log.Infof(ctx, "Taskqueuing successful")
	fmt.Fprintf(w, "Import of %s's collection successful\n", bggName)
	fmt.Fprintf(w, "Queued up %d games to be fetched.", len(gameTasks))
}

func loadGame(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	gameID := r.FormValue("gameID")
	gameXMLURL := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/thing?id=%s", gameID)
	gameHTMLURL := fmt.Sprintf("https://boardgamegeek.com/boardgame/%s", gameID)

	xmlresp, err := urlfetch.Client(ctx).Get(gameXMLURL)
	if err != nil {
		log.Errorf(ctx, "Failed to fetch xml game info: %s", err)
	}

	if xmlresp.StatusCode != http.StatusOK {
		log.Errorf(ctx, xmlresp.Status)
		http.Error(w, xmlresp.Status, xmlresp.StatusCode)
		return
	}
	xmlraw, err := ioutil.ReadAll(xmlresp.Body)
	if err != nil {
		log.Errorf(ctx, "Failed to read xml body: %s", err)
	}

	w.Header().Set("Content-Type", "text/plain")

	var gameXML GameXML
	if err := xml.Unmarshal(xmlraw, &gameXML); err != nil {
		log.Errorf(ctx, "Failed to unmarshal XML: %s", err) // TODO: Write check to ensure body has content (BGG will sometimes return w/o content)
	}
	gameXML.FetchTime = time.Now()
	for _, name := range gameXML.Names {
		if name.Type == "primary" {
			gameXML.PrimaryName = name.Name
			break
		}
	}
	if gameXML.PrimaryName == "" {
		log.Errorf(ctx, "Issue retrieving gameXML info %v", xmlresp)
		http.Error(w, "Unable to retrive game info from BGG API", http.StatusInternalServerError)
	}

	var playerPoll *Poll
	for _, poll := range gameXML.Polls {
		switch poll.Name {
		case "suggested_numplayers":
			playerPoll = poll
			/*case "suggested_playerage":
				agePoll = poll
			case "language_dependence":
				langPoll = poll
			*/
		}
	}
	var bestAt, recAt []int
	if playerPoll != nil {
		for _, playerCount := range playerPoll.Results {
			bestVotes, recVotes, nayVotes := playerCount.Votes[0].Num, playerCount.Votes[1].Num, playerCount.Votes[2].Num

			numPlayers, err := strconv.Atoi(strings.TrimSuffix(playerCount.NumPlayers, "+"))
			if err != nil {
				log.Errorf(ctx, "Failed to convert numPlayers string to int: %s", err)
			}
			if strings.HasSuffix(playerCount.NumPlayers, "+") {
				numPlayers++
			}

			if bestVotes+recVotes > nayVotes {
				if bestVotes > recVotes {
					bestAt = append(bestAt, numPlayers)
				} else {
					recAt = append(recAt, numPlayers)
				}
			}
		}
	}

	htmlresp, err := urlfetch.Client(ctx).Get(gameHTMLURL)
	if err != nil {
		log.Errorf(ctx, "Failed to download url: %s", err)
	}

	if htmlresp.StatusCode != http.StatusOK {
		log.Errorf(ctx, htmlresp.Status)
		http.Error(w, htmlresp.Status, htmlresp.StatusCode)
		return
	}

	htmlraw, err := ioutil.ReadAll(htmlresp.Body)
	if err != nil {
		log.Errorf(ctx, "Failed to read body: %s", err)
	}

	needle := []byte("GEEK.geekitemPreload")
	start := bytes.Index(htmlraw, needle)
	if start < 0 {
		log.Errorf(ctx, "Couldn't find GEEK.geekitemPreload in htmlraw")
	}
	start += len(needle)

	preload := htmlraw[start:]
	brace := bytes.IndexByte(preload, '{')
	if brace < 0 {
		log.Errorf(ctx, "Couldn't find the first brace in preloaded data")
	}

	var data struct{ Item struct{ Stats GameJSON } }
	if err := json.NewDecoder(bytes.NewReader(preload[brace:])).Decode(&data); err != nil {
		log.Errorf(ctx, "Failed to parse json")
	}

	gameJSON := data.Item.Stats

	game := &Game{
		Name:       gameXML.PrimaryName,
		Best:       bestAt,
		Rec:        recAt,
		MinPlayers: gameXML.MinPlayers.Num,
		MaxPlayers: gameXML.MaxPlayers.Num,
		Score:      gameJSON.Score,
		Weight:     gameJSON.Weight,
		BScore:     gameJSON.BScore,
		Ratings:    gameJSON.Ratings,
		FetchTime:  time.Now(),
	}

	key := datastore.NewKey(ctx, "Game", gameID, 0, nil)

	if _, err := datastore.Put(ctx, key, game); err != nil {
		log.Errorf(ctx, "Failed to store user collection: %s", err)
	}
	pretty.Fprint(w, game)

}

func watchProgress(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	queue, err := taskqueue.QueueStats(ctx, []string{"my-push-queue"})
	if err != nil {
		log.Errorf(ctx, "Failed to fetch queue stats: %s", err)
	}

	w.Header().Set("Content-Type", "text/html")
	pretty.Fprint(w, queue)
	page := `<form action="/watchprogress" method="post">
    <div>
        <input type="submit" value="Refresh"><br>
    </div>
</form>
<form action="/" method="post">
    <div>
        <input type="submit" value="Home"><br>
    </div>
</form>
`

	fmt.Fprintf(w, page)
}

func suggestedGames(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	numPlayers, err := strconv.Atoi(r.FormValue("numPlayers"))
	if err != nil {
		http.Error(w, "Please enter a valid number of players", http.StatusBadRequest)
		return
	}
	bggName := r.FormValue("bggName")

	bestQuery := datastore.NewQuery("Game").Filter("Best =", numPlayers)
	recQuery := datastore.NewQuery("Game").Filter("Rec =", numPlayers)

	var best []Game
	_, err = bestQuery.GetAll(ctx, &best)
	if err != nil {
		log.Errorf(ctx, "Failed to query games: %s", err)
		http.Error(w, "Internal error, please try again", http.StatusInternalServerError)
	}

	var rec []Game
	_, err = recQuery.GetAll(ctx, &rec)
	if err != nil {
		log.Errorf(ctx, "Failed to query games: %s", err)
		http.Error(w, "Internal error, please try again", http.StatusInternalServerError)
	}

	pretty.Fprint(w, best)
	pretty.Fprint(w, rec)
	_ = bggName
	/*
		collQuery := datastore.NewQuery("Collection").Filter("__key__ =", bggName)
		var coll []Collection
		_, err = collQuery.GetAll(ctx, coll)
		if err != nil {
			log.Errorf(ctx, "Failed to pull collection: %s", err)
			http.Error(w, "Please enter a valid Collection name", http.StatusBadRequest)
			return
		}
		var gameKeys []*datastore.Key

		for _, gameID := range coll[0].Items {
			gameKeys = append(gameKeys, datastore.NewKey(ctx, "Game", gameID.ObjectID, 0, nil))
		}

		var games = make([]*Game, len(coll.Items))
		err = datastore.GetMulti(ctx, gameKeys, games)
		if err != nil {
			log.Errorf(ctx, "Failed to pull games: %s", err)
			http.Error(w, "Sorry something went wrong fetching games", http.StatusInternalServerError)
			return
		}

		var bestGames []Game
		var recGames  []Game

		for _, game := range game {
			if game.Rec
		} */
}

const recordsPerPage = 10

func displayLogs(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)

	// Set up a data structure to pass to the HTML template.
	var data struct {
		Records []*log.Record
		Offset  string // base-64 encoded string
	}

	// Set up a log.Query.
	query := &log.Query{AppLogs: true}

	// Get the incoming offset param from the Next link to advance through
	// the logs. (The first time the page is loaded there won't be any offset.)
	if offset := r.FormValue("offset"); offset != "" {
		query.Offset, _ = base64.URLEncoding.DecodeString(offset)
	}

	// Run the query, obtaining a Result iterator.
	res := query.Run(ctx)

	// Iterate through the results populating the data struct.
	for i := 0; i < recordsPerPage; i++ {
		rec, err := res.Next()
		if err == log.Done {
			break
		}
		if err != nil {
			log.Errorf(ctx, "Reading log records: %v", err)
			break
		}

		data.Records = append(data.Records, rec)
		if i == recordsPerPage-1 {
			data.Offset = base64.URLEncoding.EncodeToString(rec.Offset)
		}
	}

	// Render the template to the HTTP response.
	w.Header().Set("Content-Type", "text/html")

	if err := tmpl.Execute(w, data); err != nil {
		log.Errorf(ctx, "Rendering template: %v", err)
	}
}

var tmpl = template.Must(template.New("").Parse(`
        {{range .Records}}
                <h2>Request Log</h2>
                <p>{{.EndTime}}: {{.IP}} {{.Method}} {{.Resource}}</p>
                {{with .AppLogs}}
                        <h3>App Logs:</h3>
                        <ul>
                        {{range .}}
                                <li>{{.Time}}: {{.Message}}</li>
                        <{{end}}
                        </ul>
                {{end}}
        {{end}}
        {{with .Offset}}
                <a href="?offset={{.}}">Next</a>
        {{end}}
`))
