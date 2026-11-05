package deepseek

// SearchRequest is the request body for POST /v1/tools/search.
type SearchRequest struct {
	Query        string  `json:"query"`
	Count        int     `json:"count"`
	OriQuestion  *string `json:"ori_question,omitempty"`
	Localization string  `json:"localization,omitempty"`
	NeedContent  bool    `json:"need_content"`
}

// SearchResult is a single entry of the search response.
type SearchResult struct {
	RerankScore float64 `json:"rerank_score"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Content     string  `json:"content"`
	SiteName    *string `json:"site_name"`
	SiteIcon    *string `json:"site_icon"`
	Date        *string `json:"date"`
	Authority   int     `json:"authority"`
}

// SearchResponse is the response of POST /v1/tools/search.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// FetchRequest is the request body for POST /v1/tools/fetch.
type FetchRequest struct {
	URL string `json:"url"`
}

// FetchResponse is the response of POST /v1/tools/fetch.
type FetchResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// BalanceInfo is one currency bucket of the account balance.
type BalanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

// BalanceResponse is the response of GET /user/balance.
type BalanceResponse struct {
	IsAvailable  bool          `json:"is_available"`
	BalanceInfos []BalanceInfo `json:"balance_infos"`
}
