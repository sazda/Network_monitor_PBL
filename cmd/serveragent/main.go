// Komanda serveragent je agent za servere: javlja konzoli da je server na
// vezi i šalje njegovo stanje (uptime, CPU, RAM, diskovi). Ne izvršava
// merenja mreže. Instalira se paketom NetworkMonitorServerAgent.msi.
//
//	serveragent -console https://monitor.example.com run
package main

import (
	"github.com/sazda/network-monitor/internal/agentcmd"
	"github.com/sazda/network-monitor/internal/protocol"
)

func main() {
	agentcmd.Main(agentcmd.Options{
		ServiceName: "network-monitor-server-agent",
		DisplayName: "Network Monitor Server Agent",
		Description: "Javlja Network Monitor konzoli stanje servera (na vezi, uptime, CPU, RAM, diskovi).",
		RegistryKey: `SOFTWARE\NetworkMonitor\ServerAgent`,
		StateSubdir: "ServerAgent",
		Role:        protocol.RoleServer,
	})
}
