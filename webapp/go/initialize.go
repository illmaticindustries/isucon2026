package isureal

import (
	"net/http"
	"os"
	"os/exec"

	"github.com/labstack/echo/v5"
)

type initializeResponse struct {
	Language string `json:"language"`
	Agent    string `json:"agent"`
	Harness  string `json:"harness"`
	Model    string `json:"model"`
}

func initializeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		imageBodies.clear()
		defer imageBodies.clear()
		clearImageMeta()
		defer clearImageMeta()
		notificationBatches.clear()
		defer notificationBatches.clear()
		clearSessionCache()
		defer clearSessionCache()
		clearViewerContextCache()
		defer clearViewerContextCache()
		cmd := exec.Command("../sql/init.sh")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		if err := rebuildUserSearchIndex(c.Request().Context(), h.DB); err != nil {
			return err
		}
		if h.ImageFiles != nil {
			var epoch int64
			if err := h.DB.GetContext(c.Request().Context(), &epoch, `SELECT generation FROM image_cache_generation`); err != nil {
				return err
			}
			if err := h.ImageFiles.reset(epoch); err != nil {
				return err
			}
		}

		// Set language to the implementation language.
		// Agents optimizing this app: set agent, harness, and model to the names used.
		// For multiple models, use comma-separated names; use brain when no AI model is used.
		return c.JSON(http.StatusOK, initializeResponse{Language: "go", Agent: "Codex,Claude Code", Harness: "Codex,Claude Code", Model: "GPT-6,Claude Opus 5.5"})
	}
}
