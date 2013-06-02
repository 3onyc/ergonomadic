package irc

import (
	"code.google.com/p/go.crypto/bcrypt"
	"database/sql"
	"log"
	"net"
	"time"
)

const (
	DEBUG_SERVER = true
)

type ChannelNameMap map[string]*Channel
type UserNameMap map[string]*User
type ServiceNameMap map[string]Service

type Server struct {
	hostname string
	ctime    time.Time
	name     string
	password []byte
	users    UserNameMap
	channels ChannelNameMap
	services ServiceNameMap
	commands chan<- Command
	db       *sql.DB
}

func NewServer(name string) *Server {
	commands := make(chan Command)
	server := &Server{
		ctime:    time.Now(),
		name:     name,
		commands: commands,
		users:    make(UserNameMap),
		channels: make(ChannelNameMap),
		services: make(ServiceNameMap),
		db:       NewDatabase(),
	}
	go server.receiveCommands(commands)
	NewNickServ(server)
	Transact(server.db, func(q Queryable) bool {
		urs, err := FindAllUsers(server.db)
		if err != nil {
			return false
		}
		for _, ur := range urs {
			NewUser(ur.nick, server).SetHash(ur.hash)
		}
		return false
	})
	return server
}

func (server *Server) receiveCommands(commands <-chan Command) {
	for command := range commands {
		if DEBUG_SERVER {
			log.Printf("%s → %s : %s", command.Client(), server, command)
		}
		command.Client().atime = time.Now()
		command.HandleServer(server)
	}
}

func (s *Server) Listen(addr string) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("Server.Listen: ", err)
	}

	s.hostname = LookupHostname(listener.Addr())
	log.Print("Server.Listen: listening on ", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Print("Server.Listen: ", err)
			continue
		}
		log.Print("Server.Listen: accepted ", conn.RemoteAddr())
		NewClient(s, conn)
	}
}

func (s *Server) GetOrMakeChannel(name string) *Channel {
	channel := s.channels[name]

	if channel == nil {
		channel = NewChannel(s, name)
		s.channels[name] = channel
	}

	return channel
}

// Send a message to clients of channels fromClient is a member.
func (s *Server) InterestedUsers(fromUser *User) UserSet {
	users := make(UserSet)
	users.Add(fromUser)
	for channel := range fromUser.channels {
		for user := range channel.members {
			users.Add(user)
		}
	}

	return users
}

// server functionality

func (s *Server) tryRegister(c *Client) {
	if !c.registered && c.HasNick() && c.HasUsername() && s.CheckPassword(c) {
		c.registered = true
		replies := []Reply{
			RplWelcome(s, c),
			RplYourHost(s, c),
			RplCreated(s),
			RplMyInfo(s),
		}
		for _, reply := range replies {
			c.Replies() <- reply
		}
	}
}

func (s *Server) CheckPassword(c *Client) bool {
	return (s.password == nil) || c.serverPass
}

func (s *Server) Id() string {
	return s.name
}

func (s *Server) String() string {
	return s.Id()
}

func (s *Server) PublicId() string {
	return s.Id()
}

func (s *Server) Nick() string {
	return s.name
}

func (s *Server) DeleteChannel(channel *Channel) {
	delete(s.channels, channel.name)
	if err := DeleteChannel(s.db, channel); err != nil {
		log.Println(err)
	}
}

//
// commands
//

func (m *UnknownCommand) HandleServer(s *Server) {
	m.Client().Replies() <- ErrUnknownCommand(s, m.command)
}

func (m *PingCommand) HandleServer(s *Server) {
	m.Client().Replies() <- RplPong(s)
}

func (m *PongCommand) HandleServer(s *Server) {
	// no-op
}

func (m *PassCommand) HandleServer(s *Server) {
	err := bcrypt.CompareHashAndPassword(s.password, []byte(m.password))
	if err != nil {
		m.Client().Replies() <- ErrPasswdMismatch(s)
		return
	}

	m.Client().serverPass = true
	// no reply?
}

func (m *NickCommand) HandleServer(s *Server) {
	c := m.Client()
	if c.user == nil {
		c.Replies() <- RplNick(c, m.nickname)
		c.nick = m.nickname
		s.tryRegister(c)
		return
	}

	user := c.user
	if s.users[m.nickname] != nil {
		user.Replies() <- ErrNickNameInUse(s, m.nickname)
		return
	}

	delete(s.users, user.nick)
	s.users[m.nickname] = user
	reply := RplNick(user, m.nickname)
	for iuser := range s.InterestedUsers(user) {
		iuser.Replies() <- reply
	}
	user.nick = m.nickname
}

func (m *UserMsgCommand) HandleServer(s *Server) {
	c := m.Client()
	if c.username != "" {
		c.Replies() <- ErrAlreadyRegistered(s)
		return
	}

	c.username, c.realname = m.user, m.realname
	s.tryRegister(c)
}

func (m *QuitCommand) HandleServer(s *Server) {
	c := m.Client()

	user := c.user
	if user != nil {
		reply := RplQuit(c, m.message)
		for user := range s.InterestedUsers(c.user) {
			user.Replies() <- reply
		}
	}
	c.conn.Close()
	if user == nil {
		return
	}

	user.LogoutClient(c)
	if !user.HasClients() {
		cmd := &PartCommand{
			BaseCommand: BaseCommand{c},
		}
		for channel := range user.channels {
			channel.Commands() <- cmd
		}
	}
}

func (m *JoinCommand) HandleServer(s *Server) {
	c := m.Client()

	if c.user == nil {
		c.Replies() <- ErrNoPrivileges(s)
		return
	}

	if m.zero {
		cmd := &PartCommand{
			BaseCommand: BaseCommand{c},
		}
		for channel := range c.user.channels {
			channel.Commands() <- cmd
		}
		return
	}

	for name := range m.channels {
		s.GetOrMakeChannel(name).Commands() <- m
	}
}

func (m *PartCommand) HandleServer(s *Server) {
	user := m.User()

	if user == nil {
		m.Client().Replies() <- ErrNoPrivileges(s)
		return
	}

	for _, chname := range m.channels {
		channel := s.channels[chname]

		if channel == nil {
			user.Replies() <- ErrNoSuchChannel(s, channel.name)
			continue
		}

		channel.Commands() <- m
	}
}

func (m *TopicCommand) HandleServer(s *Server) {
	user := m.User()

	// Hide all channels from logged-out clients.
	if user == nil {
		m.Client().Replies() <- ErrNoPrivileges(s)
		return
	}

	channel := s.channels[m.channel]
	if channel == nil {
		m.Client().Replies() <- ErrNoSuchChannel(s, m.channel)
		return
	}

	channel.Commands() <- m
}

func (m *PrivMsgCommand) HandleServer(s *Server) {
	service := s.services[m.target]
	if service != nil {
		service.Commands() <- m
		return
	}

	user := m.User()
	// Hide all users from logged-out clients.
	if user == nil {
		m.Client().Replies() <- ErrNoPrivileges(s)
		return
	}

	if m.TargetIsChannel() {
		channel := s.channels[m.target]
		if channel == nil {
			user.Replies() <- ErrNoSuchChannel(s, m.target)
			return
		}

		channel.Commands() <- m
		return
	}

	target := s.users[m.target]
	if target == nil {
		user.Replies() <- ErrNoSuchNick(s, m.target)
		return
	}

	target.Commands() <- m
}

func (m *ModeCommand) HandleServer(s *Server) {
	m.Client().Replies() <- RplUModeIs(s, m.Client())
}
