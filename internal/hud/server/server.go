package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	_ "github.com/gorilla/websocket"
	"github.com/windmilleng/wmclient/pkg/analytics"

	tiltanalytics "github.com/windmilleng/tilt/internal/analytics"
	"github.com/windmilleng/tilt/internal/cloud"
	"github.com/windmilleng/tilt/internal/hud/webview"
	"github.com/windmilleng/tilt/internal/k8s"
	"github.com/windmilleng/tilt/internal/store"
	"github.com/windmilleng/tilt/pkg/assets"
	"github.com/windmilleng/tilt/pkg/model"
)

const httpTimeOut = 5 * time.Second
const TiltTokenCookieName = "Tilt-Token"

type analyticsPayload struct {
	Verb string            `json:"verb"`
	Name string            `json:"name"`
	Tags map[string]string `json:"tags"`
}

type analyticsOptPayload struct {
	Opt string `json:"opt"`
}

type triggerPayload struct {
	ManifestNames []string `json:"manifest_names"`
}

type actionPayload struct {
	Type            string             `json:"type"`
	ManifestName    model.ManifestName `json:"manifest_name"`
	PodID           k8s.PodID          `json:"pod_id"`
	VisibleRestarts int                `json:"visible_restarts"`
}

type HeadsUpServer struct {
	store             *store.Store
	router            *mux.Router
	a                 *tiltanalytics.TiltAnalytics
	numWebsocketConns int32
	httpCli           httpClient
	cloudAddress      string
}

func ProvideHeadsUpServer(store *store.Store, assetServer assets.Server, analytics *tiltanalytics.TiltAnalytics, httpClient httpClient, cloudAddress cloud.Address) *HeadsUpServer {
	r := mux.NewRouter().UseEncodedPath()
	s := &HeadsUpServer{
		store:        store,
		router:       r,
		a:            analytics,
		httpCli:      httpClient,
		cloudAddress: string(cloudAddress),
	}

	r.HandleFunc("/api/view", s.ViewJSON)
	r.HandleFunc("/api/analytics", s.HandleAnalytics)
	r.HandleFunc("/api/analytics_opt", s.HandleAnalyticsOpt)
	r.HandleFunc("/api/trigger", s.HandleTrigger)
	r.HandleFunc("/api/action", s.DispatchAction).Methods("POST")
	r.HandleFunc("/api/snapshot/new", s.HandleNewSnapshot).Methods("POST")
	// this endpoint is only used for testing snapshots in development
	r.HandleFunc("/api/snapshot/{snapshot_id}", s.SnapshotJSON)
	r.HandleFunc("/ws/view", s.ViewWebsocket)
	r.HandleFunc("/api/user_started_tilt_cloud_registration", s.userStartedTiltCloudRegistration)
	r.HandleFunc("/snapshot_header", s.HandleSnapshotHeader).Methods("GET")

	r.PathPrefix("/").Handler(s.cookieWrapper(assetServer))

	return s
}

type funcHandler struct {
	f func(w http.ResponseWriter, r *http.Request)
}

func (fh funcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fh.f(w, r)
}

func (s *HeadsUpServer) cookieWrapper(handler http.Handler) http.Handler {
	return funcHandler{f: func(w http.ResponseWriter, r *http.Request) {
		state := s.store.RLockState()
		http.SetCookie(w, &http.Cookie{Name: TiltTokenCookieName, Value: string(state.Token), Path: "/"})
		s.store.RUnlockState()
		handler.ServeHTTP(w, r)
	}}
}

func (s *HeadsUpServer) Router() http.Handler {
	return s.router
}

func (s *HeadsUpServer) ViewJSON(w http.ResponseWriter, req *http.Request) {
	state := s.store.RLockState()
	view := webview.StateToWebView(state)
	s.store.RUnlockState()

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(view)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error rendering view payload: %v", err), http.StatusInternalServerError)
	}
}

type snapshot struct {
	View webview.View
}

func (s *HeadsUpServer) SnapshotJSON(w http.ResponseWriter, req *http.Request) {
	state := s.store.RLockState()
	view := webview.StateToWebView(state)
	s.store.RUnlockState()

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(snapshot{
		View: view,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Error rendering view payload: %v", err), http.StatusInternalServerError)
	}
}

func (s *HeadsUpServer) HandleAnalyticsOpt(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payload analyticsOptPayload

	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	opt, err := analytics.ParseOpt(payload.Opt)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing opt '%s': %v", payload.Opt, err), http.StatusBadRequest)
	}

	// only logging on opt-in, because, well, opting out means the user just told us not to report data on them!
	if opt == analytics.OptIn {
		s.a.IncrIfUnopted("analytics.opt.in")
	}

	s.store.Dispatch(store.AnalyticsOptAction{Opt: opt})
}

func (s *HeadsUpServer) HandleAnalytics(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payloads []analyticsPayload

	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&payloads)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	for _, p := range payloads {
		if p.Verb != "incr" {
			http.Error(w, "error parsing payloads: only incr verbs are supported", http.StatusBadRequest)
			return
		}

		s.a.Incr(p.Name, p.Tags)
	}
}

func (s *HeadsUpServer) DispatchAction(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payload actionPayload
	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	switch payload.Type {
	case "PodResetRestarts":
		s.store.Dispatch(
			store.NewPodResetRestartsAction(payload.PodID, payload.ManifestName, payload.VisibleRestarts))
	default:
		http.Error(w, fmt.Sprintf("Unknown action type: %s", payload.Type), http.StatusBadRequest)
	}

}

func (s *HeadsUpServer) HandleTrigger(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payload triggerPayload

	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	if len(payload.ManifestNames) != 1 {
		http.Error(w, fmt.Sprintf("/api/trigger currently supports exactly one manifest name, got %d", len(payload.ManifestNames)), http.StatusBadRequest)
		return
	}

	err = MaybeSendToTriggerQueue(s.store, payload.ManifestNames[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
}

func MaybeSendToTriggerQueue(st store.RStore, name string) error {
	mName := model.ManifestName(name)

	state := st.RLockState()
	m, ok := state.Manifest(mName)
	st.RUnlockState()

	if !ok {
		return fmt.Errorf("no manifest found with name '%s'", mName)
	}

	if m.TriggerMode != model.TriggerModeManual {
		return fmt.Errorf("can only trigger updates for manifests of TriggerModeManual")
	}

	st.Dispatch(AppendToTriggerQueueAction{Name: mName})
	return nil
}

/* -- SNAPSHOT: SENDING SNAPSHOT TO SERVER -- */
type snapshotURLJson struct {
	Url string `json:"url"`
}
type SnapshotID string

type snapshotIDResponse struct {
	ID string
}

func (s *HeadsUpServer) templateSnapshotURL(id SnapshotID) string {
	u := cloud.URL(s.cloudAddress)
	u.Path = fmt.Sprintf("snapshot/%s", id)
	return u.String()
}

func (s *HeadsUpServer) newSnapshotURL() string {
	u := cloud.URL(s.cloudAddress)
	u.Path = "/api/snapshot/new"
	return u.String()
}

func (s *HeadsUpServer) HandleNewSnapshot(w http.ResponseWriter, req *http.Request) {
	st := s.store.RLockState()
	token := st.Token
	s.store.RUnlockState()

	request, err := http.NewRequest(http.MethodPost, s.newSnapshotURL(), req.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error making request: %s", err.Error()), http.StatusInternalServerError)
		return
	}

	request.Header.Set(cloud.TiltTokenHeaderName, token.String())
	response, err := s.httpCli.Do(request)
	if err != nil {
		log.Printf("Error creating snapshot: %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, err := w.Write([]byte(err.Error()))
		if err != nil {
			log.Printf("Error writing error to response: %v\n", err)
		}
		return
	}

	responseWithID, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Printf("Error reading response when creating snapshot: %v\n", err)
		log.Printf("Error reading responseWithID: %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//unpack response with snapshot ID
	var resp snapshotIDResponse
	err = json.Unmarshal(responseWithID, &resp)
	if err != nil || resp.ID == "" {
		log.Printf("Error unpacking snapshot response JSON: %v\nJSON: %s\n", err, responseWithID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//create URL with snapshot ID
	var ID SnapshotID
	ID = SnapshotID(resp.ID)
	responsePayload := snapshotURLJson{
		Url: s.templateSnapshotURL(ID),
	}

	//encode URL to JSON format
	urlJS, err := json.Marshal(responsePayload)
	if err != nil {
		log.Printf("Error to marshal url JSON response %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	//write URL to header
	w.WriteHeader(response.StatusCode)
	_, err = w.Write(urlJS)
	if err != nil {
		log.Printf("Error writing URL response: %v\n", err)
		return
	}

}

// NB: this should be deleted and moved into TFT once we have it working
func (s *HeadsUpServer) HandleSnapshotHeader(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, err := w.Write([]byte(`
	<html>
		<head>
			<style>
				body {
					margin: 0;
				}

				header.snapshot {
					display: flex;
					align-items: center;
					width: 100%%;
					height: 40px;
				}

				svg {
					height: 30px;
					width: auto;
				}	

				h2 {
					font-family: Montserrat, sans-serif;
					font-size: 16px;
					text-transform: uppercase;
				}
			</style>
			<link href="https://fonts.googleapis.com/css?family=Montserrat:600" rel="stylesheet">
		</head>
		<body>
			<header class="snapshot">
				<a href="https://cloud.tilt.dev">
					<svg height="50" viewBox="0 0 256 50" width="256" xmlns="http://www.w3.org/2000/svg"><g fill="none" fill-rule="evenodd"><path d="m0 0h256v50h-256z" fill="#fff"/><path d="m0 0h260v50h-260z" fill="#fff"/><g fill-rule="nonzero"><g transform="translate(6 6)"><g fill="#20ba31"><path d="m104.68 0h-.394c-.608 0-1.1.486093-1.1 1.08624v9.50086c0 .6001.492 1.0862 1.1 1.0862h9.615c.607 0 1.099-.4861 1.099-1.0862v-3.9393c0-.37856-.2-.72995-.527-.92711l-9.22-5.561556c-.173-.1042788-.371-.159134-.573-.159134z"/><path d="m0 6.6478v3.9393c0 .6001.491932 1.0862 1.09929 1.0862h9.61491c.6074 0 1.0993-.4861 1.0993-1.0862v-9.50086c0-.600147-.4919-1.08624-1.0993-1.08624h-.3941c-.2022 0-.40011.0548552-.5727.159134l-9.22084 5.561556c-.327039.19716-.52656.54855-.52656.92711z"/><path d="m39.2276 1.08624v.38942c0 .19987.0555.39539.161.56593l5.6284 9.11141c.1995.3231.5551.5203.9383.5203h4.9918c.6074 0 1.0993-.4861 1.0993-1.0862v-9.50086c0-.600147-.4919-1.08624-1.0993-1.08624h-10.6202c-.6074 0-1.0993.486093-1.0993 1.08624z"/><path d="m83.6494 31.3522v-3.9393c0-.6001-.492-1.0862-1.0993-1.0862h-9.615c-.6073 0-1.0993.4861-1.0993 1.0862v9.5008c0 .6002.492 1.0863 1.0993 1.0863h.3941c.2023 0 .4002-.0549.5728-.1591l9.2208-5.5616c.327-.1977.5266-.5486.5266-.9271z"/></g><g fill="#70d37b" transform="translate(14)"><path d="m21.2113 1.08624v9.40796c0 .6001-.4919 1.0862-1.0993 1.0862h-6.1796c-.6074 0-1.0993.4861-1.0993 1.0863v24.2471c0 .6001-.4919 1.0862-1.0993 1.0862h-10.6202c-.6074 0-1.0993-.4861-1.0993-1.0862v-35.82756c0-.600147.4919-1.08624 1.0993-1.08624h18.9984c.6069 0 1.0993.486093 1.0993 1.08624z"/><path d="m38.0459 15.6408v21.273c0 .6001-.492 1.0862-1.0993 1.0862h-10.6203c-.6073 0-1.0992-.4861-1.0992-1.0862v-21.273c0-.6001.4919-1.0862 1.0992-1.0862h10.6203c.6073 0 1.0993.4866 1.0993 1.0862z"/><path d="m55.6356 26.4196v10.4942c0 .6001-.4919 1.0862-1.0993 1.0862h-10.6312c-.6074 0-1.0993-.4861-1.0993-1.0862v-35.82756c0-.600147.4919-1.08624 1.0993-1.08624h10.6202c.6074 0 1.0993.486093 1.0993 1.08624v25.33336z"/><path d="m86.986 1.08624v35.82756c0 .6001-.492 1.0862-1.0996 1.0862h-10.6202c-.6074 0-1.0993-.4861-1.0993-1.0862v-24.2471c0-.6002-.492-1.0863-1.0993-1.0863h-6.1791c-.6074 0-1.0993-.4861-1.0993-1.0862v-9.40796c0-.600147.4919-1.08624 1.0993-1.08624h18.9984c.6071 0 1.0991.486093 1.0991 1.08624z"/></g></g><path d="m108.6393 37.6655 5.6339-3.4509c.4512-.2767.7268-.7687.7268-1.2995v-31.84582c0-.59057-.477-1.06927-1.0655-1.06927h-106.38844c-.41824 0-.82852.11568-1.18533.33448l-5.633943 3.45132c-.451233.27626-.72678691.76866-.72678691 1.2991v31.84579c0 .5906.47701991 1.0693 1.06550991 1.0693h106.38839c.4183 0 .8282-.1157 1.1854-.3345z" fill="#03c7d3" transform="translate(132 7)"/><path d="m162.848 34.168c-1.328007 0-2.519995-.2799972-3.576-.84s-1.879997-1.339995-2.472-2.34-.888-2.1319937-.888-3.396.295997-2.391995.888-3.384 1.411995-1.7679972 2.46-2.328 2.243993-.84 3.588-.84c1.264006 0 2.371995.2559974 3.324.768s1.667998 1.2479952 2.148 2.208l-2.304 1.344c-.368002-.592003-.827997-1.0359985-1.38-1.332s-1.155997-.444-1.812-.444c-1.120006 0-2.047996.3639964-2.784 1.092s-1.104 1.6999939-1.104 2.916.363996 2.1879964 1.092 2.916 1.659994 1.092 2.796 1.092c.656003 0 1.259997-.1479985 1.812-.444s1.011998-.739997 1.38-1.332l2.304 1.344c-.496002.9600048-1.219995 1.6999974-2.172 2.22s-2.051994.78-3.3.78zm7.944-17.976h3v17.808h-3zm12.696 17.976c-1.296006 0-2.463995-.2799972-3.504-.84s-1.851997-1.339995-2.436-2.34-.876-2.1319937-.876-3.396.291997-2.391995.876-3.384 1.395995-1.7679972 2.436-2.328 2.207994-.84 3.504-.84c1.312007 0 2.487995.2799972 3.528.84s1.851997 1.335995 2.436 2.328.876 2.1199937.876 3.384-.291997 2.395995-.876 3.396-1.395995 1.7799972-2.436 2.34-2.215993.84-3.528.84zm0-2.568c1.104006 0 2.015996-.3679963 2.736-1.104s1.08-1.703994 1.08-2.904-.359996-2.1679963-1.08-2.904-1.631994-1.104-2.736-1.104-2.011996.3679963-2.724 1.104-1.068 1.703994-1.068 2.904.355996 2.1679963 1.068 2.904 1.619994 1.104 2.724 1.104zm12.624-10.416v6.912c0 1.1520058.259997 2.0119972.78 2.58s1.259995.852 2.22.852c1.072005 0 1.923997-.3319967 2.556-.996s.948-1.6199938.948-2.868v-6.48h3v12.816h-2.856v-1.632c-.480002.5760029-1.079996 1.0199984-1.8 1.332s-1.495996.468-2.328.468c-1.712009 0-3.059995-.4759952-4.044-1.428s-1.476-2.3639906-1.476-4.236v-7.32zm25.992-4.992v17.808h-2.88v-1.656c-.496002.608003-1.107996 1.0639985-1.836 1.368s-1.531996.456-2.412.456c-1.232006 0-2.339995-.2719973-3.324-.816s-1.755997-1.315995-2.316-2.316-.84-2.1479935-.84-3.444.279997-2.439995.84-3.432 1.331995-1.7599973 2.316-2.304 2.091994-.816 3.324-.816c.848004 0 1.623996.1439986 2.328.432s1.303998.72 1.8 1.296v-6.576zm-6.768 7.392c-.720004 0-1.367997.1639984-1.944.492s-1.031998.795997-1.368 1.404-.504 1.311996-.504 2.112.167998 1.503997.504 2.112.791997 1.0759984 1.368 1.404 1.223996.492 1.944.492 1.367997-.1639984 1.944-.492 1.031998-.795997 1.368-1.404.504-1.311996.504-2.112-.167998-1.503997-.504-2.112-.791997-1.0759984-1.368-1.404-1.223996-.492-1.944-.492z" fill="#fff"/></g></g></svg>
				</a>
				<h2>Snapshot</h2>
			</header>
		</body>
	</html>
	`))
	if err != nil {
		log.Printf("error writing response: %v\n", err)
	}
}

func (s *HeadsUpServer) userStartedTiltCloudRegistration(w http.ResponseWriter, req *http.Request) {
	s.store.Dispatch(store.UserStartedTiltCloudRegistrationAction{})
}

type httpClient interface {
	Do(*http.Request) (*http.Response, error)
}

func ProvideHttpClient() httpClient {
	return http.DefaultClient
}
