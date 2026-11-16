// Komanda agent se instalira na računare korisnika. Može da radi u prvom planu
// ili kao Windows servis.
//
//	agent -console https://monitor.example.com run
//	agent -console https://monitor.example.com install   (kao administrator/root)
//	agent start | stop | uninstall
package main

import "github.com/sazda/network-monitor/internal/agentcmd"

func main() {
	agentcmd.Main(agentcmd.Options{
		ServiceName: "network-monitor-agent",
		DisplayName: "Network Monitor Agent",
		Description: "Meri latenciju i brzinu mreže na zahtev Network Monitor konzole.",
		RegistryKey: `SOFTWARE\NetworkMonitor\Agent`,
	})
}
