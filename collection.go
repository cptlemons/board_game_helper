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
	http.Handle("/loadgame", wrapper(loadGame, "gameID"))
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

func wrapper(h http.HandlerFunc, params ...string) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for _, param := range params {
			if len(q.Get(param)) == 0 {
				http.Error(w, "missing "+param, http.StatusBadRequest)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
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
	// TODO: Make wrapper
	ctx := appengine.NewContext(r)
	bggName := r.FormValue("bggName")
	collURL := fmt.Sprintf("https://www.boardgamegeek.com/xmlapi2/collection?username=%s&excludesubtype=boardgameexpansion&own=1", bggName)

retry: //TODO: change this to be a taskqueue push
	resp, err := urlfetch.Client(ctx).Get(collURL)
	if err != nil {
		log.Errorf(ctx, "Failed to download bgg collection: %s", err)
		http.Error(w, "An error occurred. Try again.", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
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
	if _, err := taskqueue.AddMulti(ctx, gameTasks, "game-fetch"); err != nil {
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

	xmlResp, err := urlfetch.Client(ctx).Get(gameXMLURL)
	if err != nil {
		log.Errorf(ctx, "Failed to fetch xml game info: %s", err)
		return
	}
	defer xmlResp.Body.Close()

	if xmlResp.StatusCode != http.StatusOK {
		log.Errorf(ctx, xmlResp.Status)
		http.Error(w, xmlResp.Status, xmlResp.StatusCode)
		return
	}
	xmlRaw, err := ioutil.ReadAll(xmlResp.Body)
	if err != nil {
		log.Errorf(ctx, "Failed to read xml body: %s", err)
		return
	}

	var gameXML GameXML
	if err := xml.Unmarshal(xmlRaw, &gameXML); err != nil {
		log.Errorf(ctx, "Failed to unmarshal XML: %s", err)
		return
	}
	gameXML.FetchTime = time.Now()
	for _, name := range gameXML.Names {
		if name.Type == "primary" {
			gameXML.PrimaryName = name.Name
			break
		}
	}
	if gameXML.PrimaryName == "" {
		log.Errorf(ctx, "Issue retrieving gameXML info %v", xmlResp)
		http.Error(w, "Unable to retrive game info from BGG API", http.StatusInternalServerError)
		return
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
	// TODO: check votes and defer to min/max players if <n
	var bestAt, recAt []int
	if playerPoll != nil {
		for _, playerCount := range playerPoll.Results {
			bestVotes, recVotes, nayVotes := playerCount.Votes[0].Num, playerCount.Votes[1].Num, playerCount.Votes[2].Num

			// BGG can return n+ which is taken here as 1 more than the max number of players on the box
			numPlayers, err := strconv.Atoi(strings.TrimSuffix(playerCount.NumPlayers, "+"))
			if err != nil {
				log.Errorf(ctx, "Failed to convert numPlayers string to int: %s", err)
				return
			}
			if strings.HasSuffix(playerCount.NumPlayers, "+") {
				numPlayers++
			}

			if bestVotes+recVotes <= nayVotes {
				continue
			}
			if bestVotes > recVotes {
				bestAt = append(bestAt, numPlayers)
			} else {
				recAt = append(recAt, numPlayers)
			}
		}
	}

	htmlResp, err := urlfetch.Client(ctx).Get(gameHTMLURL)
	if err != nil {
		log.Errorf(ctx, "Failed to download url: %s", err)
		return
	}
	defer htmlResp.Body.Close()

	if htmlResp.StatusCode != http.StatusOK {
		log.Errorf(ctx, htmlResp.Status)
		http.Error(w, htmlResp.Status, htmlResp.StatusCode)
		return
	}

	htmlRaw, err := ioutil.ReadAll(htmlResp.Body)
	if err != nil {
		log.Errorf(ctx, "Failed to read body: %s", err)
		return
	}

	needle := []byte("GEEK.geekitemPreload")
	start := bytes.Index(htmlRaw, needle)
	if start < 0 {
		log.Errorf(ctx, "Couldn't find GEEK.geekitemPreload in htmlRaw")
		return
	}
	start += len(needle)

	preload := htmlRaw[start:]
	brace := bytes.IndexByte(preload, '{')
	if brace < 0 {
		log.Errorf(ctx, "Couldn't find the first brace in preloaded data")
		return
	}
	preload = preload[brace:]

	var data struct{ Item struct{ Stats GameJSON } }
	if err := json.NewDecoder(bytes.NewReader(preload)).Decode(&data); err != nil {
		log.Errorf(ctx, "Failed to parse json")
		return
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
		return
	}
	log.Infof(ctx, "%s", pretty.Sprint(game))

}

func watchProgress(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	queue, err := taskqueue.QueueStats(ctx, []string{"game-fetch"})
	if err != nil {
		log.Errorf(ctx, "Failed to fetch queue stats: %s", err)
		return
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

	collKey := datastore.NewKey(ctx, "Collection", bggName, 0, nil)
	var coll Collection
	err = datastore.Get(ctx, collKey, &coll)
	if err != nil {
		log.Errorf(ctx, "Failed to pull collection: %s", err)
		http.Error(w, "Please enter a valid Collection name", http.StatusBadRequest)
		return
	}
	var gameKeys []*datastore.Key

	for _, gameID := range coll.Items {
		gameKeys = append(gameKeys, datastore.NewKey(ctx, "Game", gameID.ObjectID, 0, nil))
	}

	var games = make([]*Game, len(gameKeys))
	err = datastore.GetMulti(ctx, gameKeys, games)
	if err != nil {
		log.Errorf(ctx, "Failed to pull games: %s", err)
		http.Error(w, "Sorry something went wrong fetching games", http.StatusInternalServerError)
		return
	}

	var bestGames []*Game
	var recGames []*Game

	for _, game := range games {
		for _, num := range game.Rec {
			if num == numPlayers {
				recGames = append(recGames, game)
			} else if num > numPlayers {
				break
			}
		}
		for _, num := range game.Best {
			if num == numPlayers {
				bestGames = append(bestGames, game)
			} else if num > numPlayers {
				break
			}
		}
	}
	fmt.Fprintln(w, "Best games")
	for _, game := range bestGames {
		fmt.Fprintln(w, game.Name)
	}
	fmt.Fprintln(w, "\nRec games")
	for _, game := range recGames {
		fmt.Fprintln(w, game.Name)
	}
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
