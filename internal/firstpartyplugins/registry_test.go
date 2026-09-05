package firstpartyplugins

import (
	"reflect"
	"testing"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/plugins/calendar"
	"github.com/lxdb/bsbctl/plugins/codexquota"
	"github.com/lxdb/bsbctl/plugins/macresources"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestEveryFirstPartyAppHasOnboardingCopy(t *testing.T) {
	for _, descriptor := range All() {
		value := reflect.ValueOf(descriptor)
		description := value.FieldByName("Description")
		requirement := value.FieldByName("Requirement")
		if !description.IsValid() || description.Kind() != reflect.String || description.String() == "" {
			t.Fatalf("%s has no onboarding description", descriptor.ID)
		}
		if !requirement.IsValid() || requirement.Kind() != reflect.String || requirement.String() == "" {
			t.Fatalf("%s has no onboarding requirement", descriptor.ID)
		}
	}
}

func TestRegistryOwnsEveryFirstPartyPluginIdentity(t *testing.T) {
	t.Parallel()
	wantIDs := []string{
		"dev.bsbctl.calendar",
		"dev.bsbctl.codex",
		"dev.bsbctl.codex-quota",
		"dev.bsbctl.mac-resources",
	}
	wantBinaries := []string{
		"bsbctl-plugin-calendar",
		"bsbctl-plugin-codex",
		"bsbctl-plugin-codex-quota",
		"bsbctl-plugin-mac-resources",
	}
	wantAppIDs := []string{"calendar", "codex", "codex-quota", "mac-resources"}
	descriptors := All()
	ids := make([]string, len(descriptors))
	binaries := make([]string, len(descriptors))
	appIDs := make([]string, len(descriptors))
	seenAppIDs := make(map[string]struct{}, len(descriptors))
	for index, descriptor := range descriptors {
		ids[index] = descriptor.ID
		binaries[index] = descriptor.Binary
		appIDs[index] = descriptor.DefaultApp.ID
		if descriptor.DefinitionForVersion == nil || descriptor.SchemaPath == "" || descriptor.FixturePath == "" {
			t.Fatalf("descriptor %q is incomplete: %#v", descriptor.ID, descriptor)
		}
		if (descriptor.SoakProfile == "") == (descriptor.SoakExclusion == "") {
			t.Fatalf("descriptor %q must declare exactly one soak profile or exclusion: %#v", descriptor.ID, descriptor)
		}
		definition := descriptor.DefinitionForVersion("9.8.7")
		if definition.ID != descriptor.ID || definition.Version != "9.8.7" {
			t.Fatalf("descriptor %q definition = %#v", descriptor.ID, definition)
		}
		if descriptor.DefaultApp.ID == "" || descriptor.DefaultApp.PluginID != descriptor.ID || !descriptor.DefaultApp.Enabled || string(descriptor.DefaultApp.Config) != `{}` {
			t.Fatalf("descriptor %q default app = %#v", descriptor.ID, descriptor.DefaultApp)
		}
		if _, duplicate := seenAppIDs[descriptor.DefaultApp.ID]; duplicate {
			t.Fatalf("default app %q is duplicated", descriptor.DefaultApp.ID)
		}
		seenAppIDs[descriptor.DefaultApp.ID] = struct{}{}
		app := descriptor.DefaultApp
		app.Generation = 1
		document := config.Document{
			Version: config.CurrentVersion, Generation: 1,
			Plugins: map[string]config.Plugin{descriptor.ID: {
				ID: descriptor.ID, Version: definition.Version, Executable: "/bin/true", ProtocolVersion: protocol.Version,
				ExecutionModes: definition.Contract.ExecutionModes, Channels: definition.Contract.Channels, Operations: definition.Contract.Operations,
			}},
			Apps: map[string]config.App{app.ID: app},
		}
		if err := document.Validate(); err != nil {
			t.Fatalf("descriptor %q default app is invalid: %v", descriptor.ID, err)
		}
		if found, ok := LookupID(descriptor.ID); !ok || found.ID != descriptor.ID {
			t.Fatalf("LookupID(%q) = %#v, %v", descriptor.ID, found, ok)
		}
		if found, ok := LookupBinary(descriptor.Binary); !ok || found.ID != descriptor.ID {
			t.Fatalf("LookupBinary(%q) = %#v, %v", descriptor.Binary, found, ok)
		}
		if found, ok := LookupAppID(descriptor.DefaultApp.ID); !ok || found.ID != descriptor.ID {
			t.Fatalf("LookupAppID(%q) = %#v, %v", descriptor.DefaultApp.ID, found, ok)
		}
	}
	if !reflect.DeepEqual(ids, wantIDs) || !reflect.DeepEqual(binaries, wantBinaries) || !reflect.DeepEqual(appIDs, wantAppIDs) {
		t.Fatalf("registry identities = %q / %q / %q", ids, binaries, appIDs)
	}
	if _, ok := LookupID("dev.bsbctl.unknown"); ok {
		t.Fatal("unknown plugin ID was found")
	}
	if _, ok := LookupBinary("unknown"); ok {
		t.Fatal("unknown plugin binary was found")
	}
	if _, ok := LookupAppID("unknown"); ok {
		t.Fatal("unknown default app was found")
	}
}

func TestRegistryLookupsCloneDefaultAppState(t *testing.T) {
	t.Parallel()
	descriptor, ok := LookupAppID("calendar")
	if !ok {
		t.Fatal("calendar default app was not found")
	}
	descriptor.DefaultApp.Config[0] = '['
	delete(descriptor.DefaultApp.Policies, "upcoming")

	fresh, ok := LookupAppID("calendar")
	if !ok || string(fresh.DefaultApp.Config) != `{}` || fresh.DefaultApp.Policies["upcoming"].Policy == "" {
		t.Fatalf("registry default app was mutated: %#v", fresh.DefaultApp)
	}
}

func TestCalendarDefaultUsesTheImplementedActivationAction(t *testing.T) {
	t.Parallel()
	descriptor, ok := LookupID(calendar.PluginID)
	if !ok {
		t.Fatal("Calendar descriptor not found")
	}
	if descriptor.DefaultApp.LaunchAction != calendar.CalendarOpenAction {
		t.Fatalf("Calendar launch action = %q, want %q", descriptor.DefaultApp.LaunchAction, calendar.CalendarOpenAction)
	}
	for _, channel := range []string{calendar.ChannelUpcoming, calendar.ChannelActive} {
		if action := descriptor.DefaultApp.Policies[channel].ActivationAction; action != calendar.CalendarOptionsAction {
			t.Fatalf("Calendar %s activation action = %q, want %q", channel, action, calendar.CalendarOptionsAction)
		}
	}
}

func TestResidentDashboardDefaultsAreLauncherInteractive(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		pluginID string
		channel  string
	}{
		{pluginID: codexquota.PluginID, channel: codexquota.ChannelLive},
		{pluginID: macresources.PluginID, channel: macresources.ChannelLive},
	} {
		descriptor, ok := LookupID(test.pluginID)
		if !ok {
			t.Fatalf("descriptor %q not found", test.pluginID)
		}
		if descriptor.DefaultApp.LaunchAction != "open" {
			t.Fatalf("%s launch action = %q, want open", test.pluginID, descriptor.DefaultApp.LaunchAction)
		}
		if got := descriptor.DefaultApp.Policies[test.channel].Policy; got != presentation.PolicyInteractive {
			t.Fatalf("%s live policy = %q, want %q", test.pluginID, got, presentation.PolicyInteractive)
		}
	}
}
