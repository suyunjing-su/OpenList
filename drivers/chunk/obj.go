package chunk

import (
	"sync"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

type chunkObject struct {
	model.Object
	chunkSizes []int64
	// Lazy link acquisition for performance optimization
	chunkLinks map[int]*model.Link
	linksMutex sync.RWMutex
}

// GetCachedLink returns cached link for specific chunk index
func (c *chunkObject) getCachedLink(idx int) (*model.Link, bool) {
	c.linksMutex.RLock()
	defer c.linksMutex.RUnlock()
	if c.chunkLinks == nil {
		return nil, false
	}
	link, exists := c.chunkLinks[idx]
	return link, exists
}

// SetCachedLink stores link for specific chunk index
func (c *chunkObject) setCachedLink(idx int, link *model.Link) {
	c.linksMutex.Lock()
	defer c.linksMutex.Unlock()
	if c.chunkLinks == nil {
		c.chunkLinks = make(map[int]*model.Link)
	}
	c.chunkLinks[idx] = link
}
