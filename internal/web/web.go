// Package web embeds the dashboard UI.
package web

import _ "embed"

// Index is the single-page dashboard served at /.
//
//go:embed index.html
var Index []byte

// HerdrJS and HerdrCSS implement the live, read-only Herdr monitor.
//
//go:embed herdr.js
var HerdrJS []byte

//go:embed herdr.css
var HerdrCSS []byte

// SmoothieJS is the pinned Smoothie Charts 1.36.1 runtime used by the Pulse
// monitor, with a local responsive-resize fix for device-pixel-ratio changes.
// It is served locally so the dashboard works without public internet.
//
//go:embed vendor/smoothie-1.36.1.js
var SmoothieJS []byte

// ProjectsJS and ProjectsCSS render the gh-x status section.
//
//go:embed projects.js
var ProjectsJS []byte

//go:embed projects.css
var ProjectsCSS []byte

// BillingJS and BillingCSS render metered organization billing.
//
//go:embed billing.js
var BillingJS []byte

//go:embed billing.css
var BillingCSS []byte
