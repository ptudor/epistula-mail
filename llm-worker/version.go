package main

var (
	Version   = "dev"
	BuildTime = "unknown"
)

// projectURL is the comment every User-Agent carries, so an operator reading
// an epistula-api or LM Studio access log can find the software.
const projectURL = "https://github.com/ptudor/epistula-mail"

// userAgent names this worker and build in the User-Agent of every request
// it makes, to epistula-api and to LM Studio.
func userAgent() string {
	return "epistula-llm-worker/" + Version + " (+" + projectURL + ")"
}
