package server

import (
	"errors"

	"github.com/elythia-network/elythia/plugin"
)

// registerPluginNoteDelivery runs def.NoteDelivery and adds the result to the
// registry the delivery hooks, notes/create and the AP handler share
// (#4199).
//
// プラグインの callback が panic しても起動の失敗として返す (EffectivePolicies と
// 同じ扱い)。配送先を絞るはずのプラグインが登録されないまま起動すると、絞った
// つもりのノートがどこへでも届く。
func (s *Server) registerPluginNoteDelivery(def plugin.Definition, pctx plugin.Context) (err error) {
	if s.noteDelivery == nil {
		return errors.New("note delivery registry is not wired")
	}
	reg, err := func() (reg plugin.NoteDeliveryRegistration, err error) {
		defer func() {
			if recover() != nil {
				err = errors.New("NoteDelivery panicked")
			}
		}()
		return def.NoteDelivery(pctx)
	}()
	if err != nil {
		return err
	}
	return s.noteDelivery.Add(def.Name, reg)
}
