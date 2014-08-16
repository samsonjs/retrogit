package githop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"appengine"
	"appengine/mail"
	"appengine/urlfetch"

	"code.google.com/p/goauth2/oauth"
	"github.com/google/go-github/github"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
)

var router *mux.Router
var githubOauthConfig oauth.Config
var sessionStore *sessions.CookieStore
var sessionConfig SessionConfig
var templates map[string]*template.Template

func init() {
	initTemplates()
	sessionStore, sessionConfig = initSession()
	initGithubOAuthConfig()

	router = mux.NewRouter()
	router.HandleFunc("/", indexHandler).Name("index")

	router.HandleFunc("/session/sign-in", signInHandler).Name("sign-in")
	router.HandleFunc("/session/sign-out", signOutHandler).Name("sign-out")
	router.HandleFunc("/github/callback", githubOAuthCallbackHandler)

	router.HandleFunc("/digest/view", viewDigestHandler).Name("view-digest")
	router.HandleFunc("/digest/send", sendDigestHandler).Name("send-digest").Methods("POST")
	router.HandleFunc("/digest/cron", digestCronHandler)

	router.HandleFunc("/admin/digest", digestAdminHandler)
	http.Handle("/", router)
}

func initGithubOAuthConfig() {
	path := "config/github-oauth"
	if appengine.IsDevAppServer() {
		path += "-dev"
	}
	path += ".json"
	configBytes, err := ioutil.ReadFile(path)
	if err != nil {
		log.Panicf("Could not read GitHub OAuth config from %s: %s", path, err.Error())
	}
	err = json.Unmarshal(configBytes, &githubOauthConfig)
	if err != nil {
		log.Panicf("Could not parse GitHub OAuth config %s: %s", configBytes, err.Error())
	}
	githubOauthConfig.Scope = "repo, user:email"
	githubOauthConfig.AuthURL = "https://github.com/login/oauth/authorize"
	githubOauthConfig.TokenURL = "https://github.com/login/oauth/access_token"
}

func initTemplates() {
	sharedFileNames, err := filepath.Glob("templates/shared/*.html")
	if err != nil {
		log.Panicf("Could not read shared template file names %s", err.Error())
	}
	templateFileNames, err := filepath.Glob("templates/*.html")
	if err != nil {
		log.Panicf("Could not read template file names %s", err.Error())
	}
	templates = make(map[string]*template.Template)
	for _, templateFileName := range templateFileNames {
		templateName := filepath.Base(templateFileName)
		templateName = strings.TrimSuffix(templateName, filepath.Ext(templateName))
		fileNames := make([]string, 0, len(sharedFileNames)+2)
		// The base template has to come first, except for the email template, which
		// doesn't use it
		if templateName != "digest-email" {
			fileNames = append(fileNames, "templates/base/page.html")
		}
		fileNames = append(fileNames, templateFileName)
		fileNames = append(fileNames, sharedFileNames...)
		templates[templateName], err = template.ParseFiles(fileNames...)
		if err != nil {
			log.Panicf("Could not parse template files for %s: %s", templateFileName, err.Error())
		}
	}
}

func signInHandler(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, githubOauthConfig.AuthCodeURL(""), http.StatusFound)
}

func signOutHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionStore.Get(r, sessionConfig.CookieName)
	session.Options.MaxAge = -1
	session.Save(r, w)
	indexUrl, _ := router.Get("index").URL()
	http.Redirect(w, r, indexUrl.String(), http.StatusFound)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionStore.Get(r, sessionConfig.CookieName)
	userId, ok := session.Values[sessionConfig.UserIdKey].(int)
	if !ok {
		signInUrl, _ := router.Get("sign-in").URL()
		var data = map[string]string{
			"SignInUrl": signInUrl.String(),
		}
		if err := templates["index-signed-out"].Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	c := appengine.NewContext(r)
	account, err := getAccount(c, userId)
	if account == nil {
		// Can't look up the account, session cookie must be invalid, clear it.
		indexUrl, _ := router.Get("sign-out").URL()
		http.Redirect(w, r, indexUrl.String(), http.StatusFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	signOutUrl, _ := router.Get("sign-out").URL()
	viewDigestUrl, _ := router.Get("view-digest").URL()
	sendDigestUrl, _ := router.Get("send-digest").URL()
	var data = map[string]interface{}{
		"SignOutUrl":    signOutUrl.String(),
		"ViewDigestUrl": viewDigestUrl.String(),
		"SendDigestUrl": sendDigestUrl.String(),
	}
	if err := templates["index"].Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func viewDigestHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionStore.Get(r, sessionConfig.CookieName)
	userId := session.Values[sessionConfig.UserIdKey].(int)
	c := appengine.NewContext(r)
	account, err := getAccount(c, userId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	oauthTransport := githubOAuthTransport(c)
	oauthTransport.Token = &account.OAuthToken
	githubClient := github.NewClient(oauthTransport.Client())

	digest, err := newDigest(githubClient, account)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	var data = map[string]interface{}{
		"Digest": digest,
	}
	if err := templates["digest-page"].Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func sendDigestHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionStore.Get(r, sessionConfig.CookieName)
	userId := session.Values[sessionConfig.UserIdKey].(int)
	c := appengine.NewContext(r)
	account, err := getAccount(c, userId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = sendDigestForAccount(account, c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	indexUrl, _ := router.Get("index").URL()
	http.Redirect(w, r, indexUrl.String(), http.StatusFound)
}

func digestCronHandler(w http.ResponseWriter, r *http.Request) {
	var accounts []Account
	c := appengine.NewContext(r)
	getAllAccounts(c, &accounts)
	for _, account := range accounts {
		c.Infof("Sending digest for %d...", account.GitHubUserId)
		sent, err := sendDigestForAccount(&account, c)
		if err != nil {
			c.Errorf("  Error: %s", err.Error())
		} else if sent {
			c.Infof("  Sent!")
		} else {
			c.Infof("  Not sent, digest was empty")
		}
	}
	fmt.Fprint(w, "Done")
}

func sendDigestForAccount(account *Account, c appengine.Context) (bool, error) {
	oauthTransport := githubOAuthTransport(c)
	oauthTransport.Token = &account.OAuthToken
	githubClient := github.NewClient(oauthTransport.Client())

	digest, err := newDigest(githubClient, account)
	if err != nil {
		return false, err
	}
	if digest.Empty() {
		return false, nil
	}

	var data = map[string]interface{}{
		"Styles": getDigestStyles(),
		"Digest": digest,
	}
	var digestHtml bytes.Buffer
	if err := templates["digest-email"].Execute(&digestHtml, data); err != nil {
		return false, err
	}

	emails, _, err := githubClient.Users.ListEmails(nil)
	if err != nil {
		return false, err
	}
	var primaryVerified *string
	for _, email := range emails {
		if email.Primary != nil && *email.Primary &&
			email.Verified != nil && *email.Verified {
			primaryVerified = email.Email
			break
		}
	}
	if primaryVerified == nil {
		return false, errors.New("No verified email addresses found in GitHub account")
	}

	digestMessage := &mail.Message{
		Sender:   "GitHop <mihai.parparita@gmail.com>",
		To:       []string{*primaryVerified},
		Subject:  "GitHop Digest",
		HTMLBody: digestHtml.String(),
	}
	err = mail.Send(c, digestMessage)
	return true, err
}

func getDigestStyles() template.CSS {
	b, err := ioutil.ReadFile("static/digest.css")
	if err != nil {
		log.Panicf("Could not read digest CSS: %s", err.Error())
	}
	return template.CSS(string(b[:]))
}

func githubOAuthCallbackHandler(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	c := appengine.NewContext(r)
	oauthTransport := githubOAuthTransport(c)
	token, err := oauthTransport.Exchange(code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	oauthTransport.Token = token
	githubClient := github.NewClient(oauthTransport.Client())
	user, _, err := githubClient.Users.Get("")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	account := &Account{
		GitHubUserId: *user.ID,
		OAuthToken:   *token,
	}
	err = account.put(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	session, _ := sessionStore.Get(r, sessionConfig.CookieName)
	session.Values[sessionConfig.UserIdKey] = user.ID
	session.Save(r, w)
	indexUrl, _ := router.Get("index").URL()
	http.Redirect(w, r, indexUrl.String(), http.StatusFound)
}

func digestAdminHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := strconv.Atoi(r.FormValue("user_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c := appengine.NewContext(r)
	account, err := getAccount(c, userId)
	if account == nil {
		http.Error(w, "Couldn't find account", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	oauthTransport := githubOAuthTransport(c)
	oauthTransport.Token = &account.OAuthToken
	githubClient := github.NewClient(oauthTransport.Client())

	digest, err := newDigest(githubClient, account)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	var data = map[string]interface{}{
		"Digest": digest,
	}
	if err := templates["digest-admin"].Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func githubOAuthTransport(c appengine.Context) *oauth.Transport {
	appengineTransport := &urlfetch.Transport{Context: c}
	cachingTransport := &CachingTransport{
		Transport: appengineTransport,
		Context:   c,
	}
	return &oauth.Transport{
		Config:    &githubOauthConfig,
		Transport: cachingTransport,
	}
}
