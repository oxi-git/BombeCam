package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
)

func readCameraAttributes(ctx context.Context, cc bridge.ControlChannel, camera string, keys ...string) (map[string]any, error) {
	values := make(map[string]any)
	var errs []error
	if cc == nil {
		return values, fmt.Errorf("camera readback transport unavailable")
	}
	if getter, ok := cc.(interface {
		GetAttributes(context.Context, string, ...string) (map[string]any, error)
	}); ok {
		queryKeys := append([]string(nil), keys...)
		for _, key := range keys {
			if key == "LightSW" {
				queryKeys = append(queryKeys, "WhiteLightSw")
			}
		}
		v, err := getter.GetAttributes(ctx, camera, queryKeys...)
		if err != nil {
			errs = append(errs, err)
		}
		for k, value := range v {
			values[k] = value
		}
		if _, ok := values["LightSW"]; !ok {
			if v, ok := values["WhiteLightSw"]; ok {
				values["LightSW"] = v
			}
		}
		delete(values, "WhiteLightSw")
	} else if local, ok := bridge.AsLocalControl(cc); ok {
		doc, err := local.ReportedShadow(camera)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, key := range keys {
				if v, ok := doc.State.Reported[key]; ok {
					values[key] = v
				}
			}
		}
	} else {
		for _, key := range keys {
			switch key {
			case "IrLedMode":
				v, err := cc.GetIR(ctx, camera)
				if err == nil {
					values[key] = v
				} else {
					errs = append(errs, err)
				}
			case "LedOnOff":
				v, err := cc.GetLED(ctx, camera)
				if err == nil {
					values[key] = v
				} else {
					errs = append(errs, err)
				}
			case "LightSW":
				if getter, ok := cc.(interface {
					GetLight(context.Context, string) (bool, error)
				}); ok {
					v, err := getter.GetLight(ctx, camera)
					if err == nil {
						values[key] = v
					} else {
						errs = append(errs, err)
					}
				}
			}
		}
	}
	for key, value := range values {
		n, valid := bridge.ParseCameraAttributeNumber(value)
		if key != "IrLedMode" && n > 1 {
			valid = false
		}
		if !valid {
			delete(values, key)
			errs = append(errs, fmt.Errorf("invalid %s camera readback", key))
		} else {
			values[key] = n
		}
	}
	return values, errors.Join(errs...)
}

func updateCameraObservations(mc *ManagedCamera, values map[string]any) {
	if v, ok := values["IrLedMode"]; ok {
		if n, valid := bridge.ParseIRMode(v); valid {
			mc.UpdateObservedIR(bridge.IRModeName(n))
		}
	}
	for key, update := range map[string]func(string){"LedOnOff": mc.UpdateObservedLED, "LightSW": mc.UpdateObservedLight} {
		if v, ok := values[key]; ok {
			if n, valid := parseOnOff(v); valid {
				if n == 1 {
					update("on")
				} else {
					update("off")
				}
			}
		}
	}
}

const cameraObservationMaxAge = 30 * time.Second
