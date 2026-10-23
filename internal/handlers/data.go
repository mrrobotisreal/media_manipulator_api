package handlers

import "github.com/gin-gonic/gin"

// DataHandler is filled in by the per-item delete step.
type DataHandler struct{}

func (d *DataHandler) DeleteJob(c *gin.Context)     {}
func (d *DataHandler) KeepJobLonger(c *gin.Context) {}
