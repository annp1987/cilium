// Copyright 2016-2017 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package policy

import (
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/maps/policymap"

	"github.com/sirupsen/logrus"
)

// Consumable holds all of the policies relevant to this security identity,
// including label-based policies, L4Policy, and L7 policy. A Consumable is
// shared amongst all endpoints on the same node which possess the same security
// identity.
type Consumable struct {
	// ID of the consumable (same as security ID)
	ID NumericIdentity `json:"id"`

	// Mutex protects all variables from this structure below this line
	Mutex lock.RWMutex

	// Labels are the SecurityIdentity of this consumable
	Labels *Identity `json:"labels"`

	// LabelArray contains the same labels from identity in a form of a list, used for faster lookup
	LabelArray labels.LabelArray `json:"-"`

	// Iteration policy of the Consumable
	Iteration uint64 `json:"-"`

	// IngressMaps maps the file descriptor of the BPF PolicyMap in the BPF filesystem
	// to the golang representation of the same BPF PolicyPap. Each key-value
	// pair corresponds to the BPF PolicyMap for a given endpoint.
	IngressMaps map[int]*policymap.PolicyMap `json:"-"`

	// IngressIdentities is the set of security identities from which ingress
	// traffic is allowed. The value corresponds to whether the corresponding
	// key (security identity) should be garbage collected upon policy calculation.
	IngressIdentities map[NumericIdentity]bool `json:"ingress-identities"`

	// EgressMaps maps the file descriptor of the BPF PolicyMap in the BPF filesystem
	// to the golang representation of the same BPF PolicyPap. Each key-value
	// pair corresponds to the BPF PolicyMap for a given endpoint.
	EgressMaps map[int]*policymap.PolicyMap `json:"-"`

	// EgressIdentities is the set of security identities from which egress
	// traffic is allowed. The value corresponds to whether the corresponding
	// key (security identity) should be garbage collected upon policy calculation.
	EgressIdentities map[NumericIdentity]bool `json:"egress-identities"`

	// L4Policy contains the L4-only policy of this consumable
	L4Policy *L4Policy `json:"l4-policy"`

	// L3L4Policy contains the L3, L4 and L7 ingress policy of this consumable
	L3L4Policy *SecurityIDContexts `json:"l3-l4-policy"`

	cache *ConsumableCache
}

// NewConsumable creates a new consumable
func NewConsumable(id NumericIdentity, lbls *Identity, cache *ConsumableCache) *Consumable {
	consumable := &Consumable{
		ID:                id,
		Iteration:         0,
		Labels:            lbls,
		IngressMaps:       map[int]*policymap.PolicyMap{},
		IngressIdentities: map[NumericIdentity]bool{},
		EgressMaps:        map[int]*policymap.PolicyMap{},
		EgressIdentities:  map[NumericIdentity]bool{},
		cache:             cache,
	}
	if lbls != nil {
		consumable.LabelArray = lbls.Labels.ToSlice()
	}

	return consumable
}

// AddIngressMap add m to the Consumable's IngressMaps. This represents
// the PolicyMap being added for a specific endpoint.
func (c *Consumable) AddIngressMap(m *policymap.PolicyMap) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()
	if c.IngressMaps == nil {
		c.IngressMaps = make(map[int]*policymap.PolicyMap)
	}

	// Check if map is already associated with this consumable
	if _, ok := c.IngressMaps[m.Fd]; ok {
		return
	}

	log.WithFields(logrus.Fields{
		"policymap":  m,
		"consumable": c,
	}).Debug("Adding policy map to consumable")
	c.IngressMaps[m.Fd] = m

	// Populate the new map with the already established allowed identities from
	// which ingress traffic is allowed.
	for ingressIdentity := range c.IngressIdentities {
		if err := m.AllowIdentity(ingressIdentity.Uint32()); err != nil {
			log.WithError(err).Warn("Update of policy map failed")
		}
	}
}

// AddEgressMap adds m to the Consumable's EgressMaps. This represents
// the PolicyMap being added for a specific endpoint.
func (c *Consumable) AddEgressMap(m *policymap.PolicyMap) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()
	if c.EgressMaps == nil {
		c.EgressMaps = make(map[int]*policymap.PolicyMap)
	}

	// Check if map is already associated with this consumable
	if _, ok := c.EgressMaps[m.Fd]; ok {
		return
	}

	log.WithFields(logrus.Fields{
		"policymap":  m,
		"consumable": c,
	}).Debug("Adding egress policy map to consumable")
	c.EgressMaps[m.Fd] = m

	// Populate the new map with the already established consumers of
	// this consumable
	for egressIdentity := range c.EgressIdentities {
		if err := m.AllowIdentity(egressIdentity.Uint32()); err != nil {
			log.WithError(err).Warn("Update of egress policy map failed")
		}
	}
}

func (c *Consumable) delete() {
	for ingressIdentity := range c.IngressIdentities {
		// FIXME: This explicit removal could be removed eventually to
		// speed things up as the policy map should get deleted anyway
		if c.wasLastRule(ingressIdentity) {
			c.removeFromIngressMaps(ingressIdentity)
		}
	}

	for egressIdentity := range c.IngressIdentities {
		if c.wasLastRule(egressIdentity) {
			c.removeFromEgressMaps(egressIdentity)
		}
	}

	if c.cache != nil {
		c.cache.Remove(c)
	}
}

// RemoveIngressMap removes m from the Consumable's IngressMaps. This represents
// the PolicyMap being deleted for a specific endpoint.
func (c *Consumable) RemoveIngressMap(m *policymap.PolicyMap) {
	if m != nil {
		c.Mutex.Lock()
		delete(c.IngressMaps, m.Fd)
		log.WithFields(logrus.Fields{
			"policymap":  m,
			"consumable": c,
			"count":      len(c.IngressMaps),
		}).Debug("Removing map from consumable")

		// If there are no more PolicyMaps for this Consumable, then the
		// Consumable is no longer needed and should be removed from the cache,
		// and all cross references must be undone.
		if len(c.IngressMaps) == 0 && len(c.EgressMaps) == 0 {
			c.delete()
		}
		c.Mutex.Unlock()
	}

}

// RemoveEgressMap removes m from the Consumable's EgressMaps. This represents
// the PolicyMap being deleted for a specific endpoint.
func (c *Consumable) RemoveEgressMap(m *policymap.PolicyMap) {
	if m != nil {
		c.Mutex.Lock()
		delete(c.EgressMaps, m.Fd)
		log.WithFields(logrus.Fields{
			"policymap":  m,
			"consumable": c,
			"count":      len(c.IngressMaps),
		}).Debug("Removing egress map from consumable")

		// If there are no more PolicyMaps for this Consumable, then the
		// Consumable is no longer needed and should be removed from the cache,
		// and all cross references must be undone.
		if len(c.IngressMaps) == 0 && len(c.EgressMaps) == 0 {
			c.delete()
		}
		c.Mutex.Unlock()
	}

}

func (c *Consumable) addToIngressMaps(id NumericIdentity) {
	for _, m := range c.IngressMaps {
		if m.IdentityExists(id.Uint32()) {
			continue
		}

		scopedLog := log.WithFields(logrus.Fields{
			"policymap":        m,
			logfields.Identity: id,
		})

		scopedLog.Debug("Updating ingress policy BPF map: allowing Identity")
		if err := m.AllowIdentity(id.Uint32()); err != nil {
			scopedLog.WithError(err).Warn("Update of ingress policy map failed")
		}
	}
}

func (c *Consumable) addToEgressMaps(id NumericIdentity) {
	for _, m := range c.EgressMaps {
		if m.IdentityExists(id.Uint32()) {
			continue
		}

		scopedLog := log.WithFields(logrus.Fields{
			"policymap":        m,
			logfields.Identity: id,
		})

		scopedLog.Debug("Updating egress policy BPF map: allowing Identity")
		if err := m.AllowIdentity(id.Uint32()); err != nil {
			scopedLog.WithError(err).Warn("Update of egress policy map failed")
		}
	}
}

// A rule is the 'last rule' for an identity if it does not exist as a key
// in any of the maps for this Consumable.
func (c *Consumable) wasLastRule(id NumericIdentity) bool {
	_, existsIngressIdentity := c.IngressIdentities[id]
	_, existsEgressIdentity := c.EgressIdentities[id]
	return !existsIngressIdentity && !existsEgressIdentity
}

func (c *Consumable) removeFromIngressMaps(id NumericIdentity) {
	for _, m := range c.IngressMaps {
		scopedLog := log.WithFields(logrus.Fields{
			"policymap":        m,
			logfields.Identity: id,
		})

		scopedLog.Debug("Updating ingress policy BPF map: denying Identity")
		if err := m.DeleteIdentity(id.Uint32()); err != nil {
			scopedLog.WithError(err).Warn("Update of policy map failed")
		}
	}
}

func (c *Consumable) removeFromEgressMaps(id NumericIdentity) {
	for _, m := range c.EgressMaps {
		scopedLog := log.WithFields(logrus.Fields{
			"policymap":        m,
			logfields.Identity: id,
		})

		scopedLog.Debug("Updating egress policy BPF map: denying Identity")
		if err := m.DeleteIdentity(id.Uint32()); err != nil {
			scopedLog.WithError(err).Warn("Update of policy map failed")
		}
	}
}

// AllowIngressIdentityLocked adds the given security identity to the Consumable's
// IngressIdentities map. Must be called with Consumable mutex Locked.
// Returns true if the identity was not present in this Consumable's
// IngressIdentities map, and thus had to be added, false if it is already added.
func (c *Consumable) AllowIngressIdentityLocked(cache *ConsumableCache, id NumericIdentity) bool {
	_, exists := c.IngressIdentities[id]
	if !exists {
		log.WithFields(logrus.Fields{
			logfields.Identity: id,
			"consumable":       logfields.Repr(c),
		}).Debug("Allowing security identity on ingress for consumable")
		c.addToIngressMaps(id)

		// If id corresponds to a reserved identity, Consumable corresponding to
		// that security identity needs to be updated explicitly, as reserved
		// identities do not have a corresponding endpoint for which policy
		// recalculation (when Consumables are updated) is done.
		if id.IsReservedIdentity() {
			reservedConsumable := cache.Lookup(id)
			if reservedConsumable != nil {
				// Avoid deadlock ; is this necessary? Being cautious.
				if id != c.ID {
					reservedConsumable.Mutex.Lock()
					reservedConsumable.AllowIngressIdentityLocked(cache, c.ID)
					reservedConsumable.Mutex.Unlock()
				} else {
					reservedConsumable.AllowIngressIdentityLocked(cache, c.ID)
				}
			} else {
				log.WithField(logfields.Identity, id).Warningf("unable to allow ingress from identity %d", c.ID)
			}
		}
	}

	c.IngressIdentities[id] = true

	return !exists // not changed, was already in map.
}

// AllowEgressConsumerLocked adds the given consumer ID to the Consumable's
// consumers map. Must be called with Consumable mutex Locked.
// Returns true if the consumer was not present in this Consumable's consumer map,
// and thus had to be added, false if it is already added.
func (c *Consumable) AllowEgressIdentityLocked(cache *ConsumableCache, id NumericIdentity) bool {
	_, exists := c.EgressIdentities[id]
	if !exists {
		log.WithFields(logrus.Fields{
			logfields.Identity: id,
			"consumable":       logfields.Repr(c),
		}).Debug("New egress security identity for consumable")
		c.addToEgressMaps(id)
	}
	c.EgressIdentities[id] = true
	return !exists // not changed.
}

// RemoveIngressIdentityLocked removes the given security identity from Consumable's
// IngressIdentities map.
// Must be called with the Consumable mutex locked.
func (c *Consumable) RemoveIngressIdentityLocked(id NumericIdentity) {
	if _, ok := c.IngressIdentities[id]; ok {
		log.WithField(logfields.Identity, id).Debug("Removing identity from ingress map")
		delete(c.IngressIdentities, id)

		// Consumables corresponding to reserved identities need to be updated
		// explicitly because they are not updated or regenerated.
		if id.IsReservedIdentity() {
			reservedConsumable := c.cache.Lookup(id)
			if reservedConsumable != nil {
				// Avoid deadlock!
				if id != c.ID {
					reservedConsumable.Mutex.Lock()
					reservedConsumable.RemoveIngressIdentityLocked(c.ID)
					reservedConsumable.Mutex.Unlock()
				} else {
					reservedConsumable.RemoveIngressIdentityLocked(c.ID)
				}
			} else {
				log.WithField(logfields.Identity, id).Warningf("unable to disallow ingress from identity %d", c.ID)
			}

		}
		if c.wasLastRule(id) {
			c.removeFromIngressMaps(id)
		}
	}
}

// RemoveEgressIdentityLocked removes the given security identity from Consumable's
// EgressIdentities map.
// Must be called with the Consumable mutex locked.
func (c *Consumable) RemoveEgressIdentityLocked(id NumericIdentity) {
	if _, ok := c.EgressIdentities[id]; ok {
		log.WithField(logfields.Identity, id).Debug("Removing egress identity")
		delete(c.EgressIdentities, id)

		if c.wasLastRule(id) {
			c.removeFromEgressMaps(id)
		}
	}
}

func (c *Consumable) Allows(id NumericIdentity) bool {
	c.Mutex.RLock()
	isIdentityAllowed, _ := c.IngressIdentities[id]
	c.Mutex.RUnlock()
	return isIdentityAllowed
}
